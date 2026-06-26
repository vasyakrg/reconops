package investigator

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/vasyakrg/recon/internal/hub/logtriage"
)

// defaultMaxResultTokens bounds the assembled tool-result JSON the LLM sees
// after a collect / collect_batch (and, via Task 5, search_artifact). PROJECT.md
// §7.4 targets ~500–2000 tokens per result; a 50-host fleet survey that inlines
// a full logtriage.ArtifactIndex per task would otherwise be ~25–50K tokens in
// a single message. The cap is the single source of truth for per-result and
// search-result budgets; it is configurable via llm.max_result_tokens.
const defaultMaxResultTokens = 2000

// budgetExampleCap is the per-cluster Example rune length demotion step 1 uses
// before falling back to dropping line samples / collapsing indexes.
const budgetExampleCap = 80

// rollupMaxHostsPerCluster bounds the per-cluster host breakdown a batch
// roll-up carries before trimRollup tightens it under budget pressure.
const rollupMaxHostsPerCluster = 8

// budgetHint steers the model back to the retrieval tools whenever a result was
// trimmed to fit the budget — the full structured data is always recoverable.
const budgetHint = "result truncated to fit token budget — call get_full_result(task_id) for full structured data or search_artifact(task_id, pattern) for raw matches"

// resultBudgetOutcome reports what the demotion ladder did, for verbose logging.
// It never carries raw artifact bodies.
type resultBudgetOutcome struct {
	EstimatedTokens int
	MaxTokens       int
	FinalTokens     int
	EstimatedBytes  int
	FinalBytes      int
	DemotionSteps   []string
	OmittedTasks    int
	Demoted         bool
}

// resultTokens estimates the LLM-facing token cost of a ToolResult whose Data
// is the supplied payload. It marshals the same shape executeApproved sends on
// the wire ({"ok":true,"data":...}) so the estimate matches reality. Shared by
// the per-result budget and (Task 5) the search_artifact cap.
func resultTokens(data any) (tokens, bytesLen int) {
	body, err := json.Marshal(ToolResult{OK: true, Data: data})
	if err != nil {
		return 0, 0
	}
	return tokensForBytes(len(body)), len(body)
}

// applyResultBudget progressively demotes the heaviest parts of a collect /
// collect_batch task-view slice until the assembled JSON is within maxTokens.
// It mutates views in place — that is safe because SummarizeTasks rebuilds the
// views from the store on every call, and the stored result stays full so the
// model can re-read it via get_full_result(task_id) / search_artifact. It
// returns the top-level markers to merge into the result plus an outcome for
// logging.
//
// Demotion ladder — each step is applied wholesale, then the cost is
// re-measured; we stop as soon as we are under budget:
//  1. shorten every cluster Example string;
//  2. drop FirstLines / LastLines from every artifact_index;
//  3. collapse each artifact_index to a headline (name, size_bytes,
//     line_count, top-1 severity cluster + count) and flag _index_truncated;
//  4. drop whole per-task entries from the tail, counting _omitted_tasks.
func applyResultBudget(views []taskView, maxTokens int) ([]taskView, map[string]any, resultBudgetOutcome) {
	if maxTokens <= 0 {
		maxTokens = defaultMaxResultTokens
	}
	outcome := resultBudgetOutcome{MaxTokens: maxTokens}
	meta := map[string]any{}

	estimate := func(vs []taskView) (int, int) {
		payload := map[string]any{"tasks": vs}
		for k, v := range meta {
			payload[k] = v
		}
		return resultTokens(payload)
	}

	outcome.EstimatedTokens, outcome.EstimatedBytes = estimate(views)
	outcome.FinalTokens, outcome.FinalBytes = outcome.EstimatedTokens, outcome.EstimatedBytes
	if outcome.EstimatedTokens <= maxTokens {
		return views, meta, outcome
	}

	// Any demotion path steers the model back to the retrieval tools.
	meta["_hint"] = budgetHint

	finish := func(step string) ([]taskView, map[string]any, resultBudgetOutcome) {
		if step != "" {
			outcome.DemotionSteps = append(outcome.DemotionSteps, step)
		}
		outcome.FinalTokens, outcome.FinalBytes = estimate(views)
		outcome.Demoted = true
		return views, meta, outcome
	}

	under := func() bool {
		t, _ := estimate(views)
		return t <= maxTokens
	}

	// Step 1: shorten cluster examples.
	for i := range views {
		for j := range views[i].ArtifactIndex {
			for k := range views[i].ArtifactIndex[j].TopPatterns {
				views[i].ArtifactIndex[j].TopPatterns[k].Example =
					truncateRunes(views[i].ArtifactIndex[j].TopPatterns[k].Example, budgetExampleCap)
			}
		}
	}
	outcome.DemotionSteps = append(outcome.DemotionSteps, "shorten_examples")
	if under() {
		return finish("")
	}

	// Step 2: drop head/tail line samples.
	for i := range views {
		for j := range views[i].ArtifactIndex {
			views[i].ArtifactIndex[j].FirstLines = nil
			views[i].ArtifactIndex[j].LastLines = nil
		}
	}
	outcome.DemotionSteps = append(outcome.DemotionSteps, "drop_line_samples")
	if under() {
		return finish("")
	}

	// Step 3: collapse each artifact_index to a headline.
	for i := range views {
		if len(views[i].ArtifactIndex) == 0 {
			continue
		}
		for j := range views[i].ArtifactIndex {
			views[i].ArtifactIndex[j] = headlineIndex(views[i].ArtifactIndex[j])
		}
		views[i].IndexTruncated = true
	}
	outcome.DemotionSteps = append(outcome.DemotionSteps, "collapse_index")
	if under() {
		return finish("")
	}

	// Step 4: drop whole per-task entries from the tail. Keep at least one so
	// the model always sees a navigable anchor (task_id + status).
	for len(views) > 1 {
		views = views[:len(views)-1]
		outcome.OmittedTasks++
		meta["_omitted_tasks"] = outcome.OmittedTasks
		if under() {
			return finish("drop_tasks")
		}
	}
	return finish("drop_tasks")
}

// headlineIndex reduces an artifact index to navigation-only metadata: name,
// size, line count, binary/truncated flags, and the single top severity
// cluster (template + count + severity). Everything else (line samples, units,
// time range, the rest of TopPatterns) is recoverable via get_full_result /
// search_artifact.
func headlineIndex(idx logtriage.ArtifactIndex) logtriage.ArtifactIndex {
	h := logtriage.ArtifactIndex{
		Name:      idx.Name,
		SizeBytes: idx.SizeBytes,
		LineCount: idx.LineCount,
		Binary:    idx.Binary,
		Truncated: idx.Truncated,
	}
	if len(idx.TopPatterns) > 0 {
		top := idx.TopPatterns[0]
		h.TopPatterns = []logtriage.Cluster{{
			Template: truncateRunes(top.Template, budgetExampleCap),
			Count:    top.Count,
			Severity: top.Severity,
		}}
	}
	return h
}

// searchMatch is one search_artifact hit. It is package-level so the search
// output budget (Task 5) can trim a match set with the same token cap the
// per-result budget (Task 1) uses.
type searchMatch struct {
	LineNo  int      `json:"line"`
	Text    string   `json:"text"`
	Context []string `json:"context,omitempty"`
}

// searchBudgetHint steers the model to a tighter query when a search result was
// capped by the token budget rather than by max_matches.
const searchBudgetHint = "search output capped by token budget — narrow the regex or lower context_lines; line refs on returned matches let you re-search a specific region"

// capSearchMatches trims a search_artifact match set so the assembled result
// stays within maxTokens, reusing the per-result cap as the single source of
// truth. It first drops context lines from every match, then drops whole
// matches from the tail (keeping at least one). Line refs (LineNo) and match
// text are always preserved on the matches that remain. Returns the kept
// matches, how many were omitted, whether context was dropped, and the ladder
// taken (for logging). It mutates the input slice's elements (caller owns it).
func capSearchMatches(matches []searchMatch, fixed map[string]any, maxTokens int) (kept []searchMatch, omitted int, droppedContext bool, steps []string) {
	if maxTokens <= 0 {
		maxTokens = defaultMaxResultTokens
	}
	estimate := func(ms []searchMatch, extra map[string]any) int {
		payload := map[string]any{}
		for k, v := range fixed {
			payload[k] = v
		}
		for k, v := range extra {
			payload[k] = v
		}
		payload["matches"] = ms
		payload["count"] = len(ms)
		t, _ := resultTokens(payload)
		return t
	}
	if estimate(matches, nil) <= maxTokens {
		return matches, 0, false, nil
	}

	// Step 1: drop context lines — usually the bulk of a wide search.
	hasContext := false
	for i := range matches {
		if len(matches[i].Context) > 0 {
			hasContext = true
		}
		matches[i].Context = nil
	}
	if hasContext {
		droppedContext = true
		steps = append(steps, "drop_context")
	}
	hint := map[string]any{"_hint": searchBudgetHint}
	if estimate(matches, hint) <= maxTokens {
		return matches, 0, droppedContext, steps
	}

	// Step 2: drop matches from the tail, keeping a navigable anchor.
	for len(matches) > 1 {
		matches = matches[:len(matches)-1]
		omitted++
		if estimate(matches, map[string]any{"_hint": searchBudgetHint, "omitted_matches": omitted}) <= maxTokens {
			break
		}
	}
	steps = append(steps, "drop_matches")
	return matches, omitted, droppedContext, steps
}

// stripPatterns reduces an artifact index to a per-host drill-in headline for
// the batch roll-up path: name, size, line_count, time range, units, and the
// binary/truncated flags survive; the repeated TopPatterns and line samples are
// dropped because the cross-host roll-up already carries them.
func stripPatterns(idx logtriage.ArtifactIndex) logtriage.ArtifactIndex {
	idx.TopPatterns = nil
	idx.FirstLines = nil
	idx.LastLines = nil
	return idx
}

// trimRollup reduces a batch roll-up until its standalone token cost is within
// maxTokens. Deterministic ladder: shorten examples/templates -> drop
// per-cluster host-breakdown tails -> drop the lowest-ranked clusters. The
// caller has already severity/count-ordered the clusters, so dropping from the
// tail removes the least-severe, lowest-volume ones first.
func trimRollup(r *batchRollup, maxTokens int) {
	if r == nil {
		return
	}
	if maxTokens <= 0 {
		maxTokens = defaultMaxResultTokens / 2
	}
	for i := range r.Clusters {
		r.Clusters[i].Example = truncateRunes(r.Clusters[i].Example, budgetExampleCap)
		r.Clusters[i].Template = truncateRunes(r.Clusters[i].Template, budgetExampleCap*2)
	}
	within := func() bool {
		t, _ := resultTokens(map[string]any{"batch_rollup": r})
		return t <= maxTokens
	}
	if within() {
		return
	}
	// Tighten the per-cluster host breakdown progressively.
	for capN := 4; capN >= 1; capN-- {
		for i := range r.Clusters {
			if len(r.Clusters[i].PerHost) > capN {
				r.Clusters[i].OmittedHosts += len(r.Clusters[i].PerHost) - capN
				r.Clusters[i].PerHost = r.Clusters[i].PerHost[:capN]
			}
		}
		if within() {
			return
		}
	}
	// Drop the lowest-ranked clusters last; keep at least one.
	for len(r.Clusters) > 1 {
		r.OmittedClusters++
		r.Clusters = r.Clusters[:len(r.Clusters)-1]
		if within() {
			return
		}
	}
}

// truncateRunes shortens s to at most n runes, appending an ellipsis when it
// trims. n <= 0 returns the string unchanged.
func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}
		count++
	}
	return s
}
