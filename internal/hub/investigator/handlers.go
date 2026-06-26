package investigator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/hub/logtriage"
	"github.com/vasyakrg/recon/internal/hub/runner"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// HandlerEnv carries the dependencies tool handlers need: storage for
// hosts/runs/results, the hub runner for dispatching collect requests, and
// the api server for online-status.
type HandlerEnv struct {
	Store           *store.Store
	Runner          *runner.Runner
	Online          func(string) bool
	OnlineAgents    func() []string
	InvestigationID string
	ArtifactDir     string
	// AllowedHosts: when non-empty, list_hosts only surfaces these and
	// collect / collect_batch reject any host_id outside the set. Empty
	// preserves the legacy behaviour ("all hosts").
	AllowedHosts []string
	// AttachedPriors are the prior investigation ids attached to THIS run
	// (auto host-overlap + operator-selected). recall_prior is gated to this
	// set so the model can pull a referenced prior's full conclusion/findings
	// but cannot fish arbitrary investigations.
	AttachedPriors []string
	// Bus, when non-nil, receives finding.added events fired from
	// handleAddFinding so remote API subscribers see the finding without
	// polling. Nil-safe — Bus.Publish itself handles the nil receiver.
	Bus *Bus
	// MaxResultTokens caps the assembled collect / collect_batch /
	// search_artifact tool result the LLM sees (Task 1). 0 falls back to
	// defaultMaxResultTokens. Sourced from llm.max_result_tokens.
	MaxResultTokens int
	// Log, when non-nil, receives verbose token-economy diagnostics (result
	// budget demotion, etc.). Never receives raw artifact bodies. Nil-safe.
	Log *slog.Logger
	// OperatorApprovedClose is set by the loop when the mark_done being executed
	// was EXPLICITLY approved or edited by the operator (the "Approve & close" /
	// "Edit & close" buttons) — as opposed to the model's own proposal under an
	// armed auto-run. An explicit human approval is the strongest operator
	// directive (CLAUDE.md invariant 4), so the model-facing backstop gates
	// (coverage / explanation) stand down for it exactly as they do for OPERATOR
	// FINALIZE. They exist to stop the MODEL from closing prematurely, never to
	// override a human who reviewed the conclusion and chose to close. Without
	// this, an operator-approved close was bounced back to the LLM and the loop
	// spun — "approve & close doesn't close, it re-queries the model."
	OperatorApprovedClose bool
}

// priorAttached reports whether id is one of the priors attached to this run —
// the allowlist recall_prior is gated to.
func (e HandlerEnv) priorAttached(id string) bool {
	for _, p := range e.AttachedPriors {
		if p == id {
			return true
		}
	}
	return false
}

// inAllowed returns true if the host is in the allowlist; when the allowlist
// is empty the call is unrestricted.
func (e HandlerEnv) inAllowed(hostID string) bool {
	if len(e.AllowedHosts) == 0 {
		return true
	}
	for _, h := range e.AllowedHosts {
		if h == hostID {
			return true
		}
	}
	return false
}

// ToolResult is what gets serialized into the LLM 'tool' message.
type ToolResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  any    `json:"data,omitempty"`
}

func okResult(data any) ToolResult { return ToolResult{OK: true, Data: data} }
func errResult(err error) ToolResult {
	return ToolResult{OK: false, Error: err.Error()}
}

// Dispatch runs the named tool with the given JSON-encoded arguments and
// returns a ToolResult ready to be JSON-marshalled into a tool message.
func Dispatch(ctx context.Context, env HandlerEnv, tool, argsJSON string) ToolResult {
	switch tool {
	case "list_hosts":
		return handleListHosts(ctx, env, argsJSON)
	case "list_collectors":
		return handleListCollectors(ctx, env, argsJSON)
	case "describe_collector":
		return handleDescribeCollector(ctx, env, argsJSON)
	case "collect":
		return handleCollect(ctx, env, argsJSON)
	case "collect_batch":
		return handleCollectBatch(ctx, env, argsJSON)
	case "search_artifact":
		return handleSearchArtifact(ctx, env, argsJSON)
	case "compare_across_hosts":
		return handleCompareAcrossHosts(ctx, env, argsJSON)
	case "get_full_result":
		return handleGetFullResult(ctx, env, argsJSON)
	case "recall_prior":
		return handleRecallPrior(ctx, env, argsJSON)
	case "add_finding":
		return handleAddFinding(ctx, env, argsJSON)
	case "ask_operator":
		return handleAskOperator(ctx, env, argsJSON)
	case "mark_done":
		return handleMarkDone(ctx, env, argsJSON)
	}
	return errResult(fmt.Errorf("unknown tool %q", tool))
}

// ---- Discovery ----------------------------------------------------------

func handleListHosts(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	var args struct{ Selector string }
	_ = json.Unmarshal([]byte(argsJSON), &args)

	hosts, err := env.Store.ListHosts(ctx)
	if err != nil {
		return errResult(err)
	}
	sel := parseSelector(args.Selector)

	type hostView struct {
		ID         string            `json:"id"`
		Status     string            `json:"status"`
		Labels     map[string]string `json:"labels"`
		Facts      map[string]string `json:"facts,omitempty"`
		LastSeen   string            `json:"last_seen"`
		Online     bool              `json:"online"`
		Collectors []string          `json:"collectors,omitempty"`
	}
	out := []hostView{}
	for _, h := range hosts {
		if !env.inAllowed(h.ID) {
			continue
		}
		if !matchSelector(h.Labels, sel) {
			continue
		}
		mans, _ := env.Store.ListCollectorManifests(ctx, h.ID)
		names := make([]string, 0, len(mans))
		for _, m := range mans {
			names = append(names, m.Name)
		}
		out = append(out, hostView{
			ID:         h.ID,
			Status:     h.Status,
			Labels:     h.Labels,
			Facts:      h.Facts,
			LastSeen:   h.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
			Online:     env.Online != nil && env.Online(h.ID),
			Collectors: names,
		})
	}
	return okResult(map[string]any{"hosts": out, "count": len(out)})
}

func handleListCollectors(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	var args struct{ Category string }
	_ = json.Unmarshal([]byte(argsJSON), &args)

	hosts, err := env.Store.ListHosts(ctx)
	if err != nil {
		return errResult(err)
	}
	type entry struct {
		Name        string   `json:"name"`
		Category    string   `json:"category"`
		Version     string   `json:"version"`
		Description string   `json:"description"`
		Hosts       []string `json:"hosts"`
	}
	byName := map[string]*entry{}
	for _, h := range hosts {
		mans, _ := env.Store.ListCollectorManifests(ctx, h.ID)
		for _, m := range mans {
			var env map[string]any
			_ = json.Unmarshal(m.ManifestJSON, &env)
			cat, _ := env["category"].(string)
			desc, _ := env["description"].(string)
			if args.Category != "" && cat != args.Category {
				continue
			}
			e, ok := byName[m.Name]
			if !ok {
				e = &entry{Name: m.Name, Category: cat, Version: m.Version, Description: desc}
				byName[m.Name] = e
			}
			e.Hosts = append(e.Hosts, h.ID)
		}
	}
	out := make([]*entry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return okResult(map[string]any{"collectors": out, "count": len(out)})
}

func handleDescribeCollector(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	var args struct{ Name string }
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errResult(err)
	}
	if args.Name == "" {
		return errResult(fmt.Errorf("name required"))
	}
	hosts, _ := env.Store.ListHosts(ctx)
	for _, h := range hosts {
		mans, _ := env.Store.ListCollectorManifests(ctx, h.ID)
		for _, m := range mans {
			if m.Name == args.Name {
				var envelope map[string]any
				_ = json.Unmarshal(m.ManifestJSON, &envelope)
				return okResult(envelope)
			}
		}
	}
	return errResult(fmt.Errorf("collector %q not found on any host", args.Name))
}

// ---- Action -------------------------------------------------------------

type collectArgs struct {
	HostID         string   `json:"host_id"`
	Collector      string   `json:"collector"`
	Params         paramMap `json:"params"`
	TimeoutSeconds int32    `json:"timeout_seconds"`
}

type collectBatchArgs struct {
	HostIDs        []string `json:"host_ids"`
	Collector      string   `json:"collector"`
	Params         paramMap `json:"params"`
	TimeoutSeconds int32    `json:"timeout_seconds"`
}

// paramMap is the collect/collect_batch params map. The wire contract to the
// collector is map<string,string> (proto, runner.RunRequest.Params), but models
// routinely send JSON scalars — kernel:true, tail_lines:200 — even though the
// tool schema declares string values. A plain map[string]string unmarshal then
// fails the whole tool call with "json: cannot unmarshal bool into Go value of
// type string" (observed: inv_a00000000001 seq 16). paramMap coerces any JSON
// scalar to its canonical string form AT unmarshal, preserving the wire
// contract. Numbers are decoded via json.Number (UseNumber) so a large int like
// max_bytes:1048576 keeps its exact digits — fmt.Sprint on a float64 would emit
// "1.048576e+06" and break the collector. Its underlying type is
// map[string]string, so it assigns directly to runner.RunRequest.Params.
type paramMap map[string]string

func (p *paramMap) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case nil:
			// drop explicit null params rather than sending "<nil>"
		case string:
			m[k] = t
		default:
			// json.Number (exact digits via UseNumber), bool ("true"/"false"),
			// and any defensive fallback all stringify correctly here.
			m[k] = fmt.Sprint(t)
		}
	}
	*p = m
	return nil
}

// CollectExecution is what the loop returns for tools that ran a real run.
// It includes the run/task ids so the loop can wait for completion before
// summarizing back to the LLM.
type CollectExecution struct {
	RunID   string
	TaskIDs []string
}

// PrepareCollect is invoked from the loop AFTER operator approval. It does
// NOT return the LLM-facing summary — only kicks off the run and returns the
// task ids the loop will poll.
func PrepareCollect(ctx context.Context, env HandlerEnv, argsJSON string) (CollectExecution, error) {
	var a collectArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return CollectExecution{}, err
	}
	if a.HostID == "" || a.Collector == "" {
		return CollectExecution{}, fmt.Errorf("host_id and collector required")
	}
	if !env.inAllowed(a.HostID) {
		return CollectExecution{}, fmt.Errorf("host_id %q is outside this investigation's allowlist (%v)", a.HostID, env.AllowedHosts)
	}
	runID, err := env.Runner.CreateRun(ctx, runner.RunRequest{
		Name:      fmt.Sprintf("inv:%s %s on %s", env.InvestigationID, a.Collector, a.HostID),
		HostIDs:   []string{a.HostID},
		Collector: a.Collector,
		Params:    a.Params,
		Timeout:   a.TimeoutSeconds,
		CreatedBy: "investigator:" + env.InvestigationID,
	})
	if err != nil {
		return CollectExecution{}, err
	}
	tasks, _ := env.Store.ListTasks(ctx, runID)
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return CollectExecution{RunID: runID, TaskIDs: ids}, nil
}

func PrepareCollectBatch(ctx context.Context, env HandlerEnv, argsJSON string) (CollectExecution, error) {
	var a collectBatchArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return CollectExecution{}, err
	}
	if len(a.HostIDs) == 0 || a.Collector == "" {
		return CollectExecution{}, fmt.Errorf("host_ids and collector required")
	}
	for _, h := range a.HostIDs {
		if !env.inAllowed(h) {
			return CollectExecution{}, fmt.Errorf("host_id %q is outside this investigation's allowlist (%v)", h, env.AllowedHosts)
		}
	}
	runID, err := env.Runner.CreateRun(ctx, runner.RunRequest{
		Name:      fmt.Sprintf("inv:%s %s on %d hosts", env.InvestigationID, a.Collector, len(a.HostIDs)),
		HostIDs:   a.HostIDs,
		Collector: a.Collector,
		Params:    a.Params,
		Timeout:   a.TimeoutSeconds,
		CreatedBy: "investigator:" + env.InvestigationID,
	})
	if err != nil {
		return CollectExecution{}, err
	}
	tasks, _ := env.Store.ListTasks(ctx, runID)
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return CollectExecution{RunID: runID, TaskIDs: ids}, nil
}

// taskView is the per-task projection the LLM sees after a collect /
// collect_batch completes. It is a package-level type so the token-budget
// helper (result_budget.go) can demote its heaviest parts before it reaches
// the model.
type taskView struct {
	TaskID     string `json:"task_id"`
	HostID     string `json:"host_id"`
	Collector  string `json:"collector"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// CollectedAt is when this collect actually ran (RFC3339). It is the anchor
	// the model needs to derive incident/boot time from a relative field like
	// system_info.uptime_sec — without it, "now − uptime_sec" drifts over a
	// long-running investigation.
	CollectedAt string   `json:"collected_at,omitempty"`
	Error       string   `json:"error,omitempty"`
	Summary     any      `json:"summary,omitempty"`
	Hints       []any    `json:"hints,omitempty"`
	Artifacts   []string `json:"artifacts,omitempty"`
	// ArtifactIndex surfaces compact, severity/template-clustered metadata
	// for log artifacts (Task 11) so the model navigates via the index +
	// search_artifact instead of pulling raw bodies into context.
	ArtifactIndex []logtriage.ArtifactIndex `json:"artifact_index,omitempty"`
	// IndexTruncated is set by the token-budget helper when this task's
	// artifact_index was collapsed to a headline to fit the result cap.
	IndexTruncated bool `json:"_index_truncated,omitempty"`
}

// SummarizeTasks produces the compact JSON the LLM sees after a collect /
// collect_batch completes. Goal is ≤2K tokens (PROJECT.md §7.4); the
// result-level token budget (Task 1) enforces it before the result is sent.
// When a collect_batch ran the same log collector across several hosts, the
// repeated per-host clusters are rolled up into one cross-host summary (Task 2)
// before budgeting.
func SummarizeTasks(ctx context.Context, env HandlerEnv, taskIDs []string) ToolResult {
	out := buildTaskViews(ctx, env, taskIDs)
	maxTok := env.MaxResultTokens

	if rollup, stats, applied, differ := maybeBatchRollup(out, rollupMaxHostsPerCluster); applied {
		return summarizeWithRollup(env, out, rollup, stats, maxTok)
	} else if differ && env.Log != nil {
		env.Log.Warn("batch roll-up skipped — collectors differ across tasks",
			"investigation_id", env.InvestigationID, "tasks", len(out))
	}

	views, meta, outcome := applyResultBudget(out, maxTok)
	payload := map[string]any{"tasks": views}
	for k, v := range meta {
		payload[k] = v
	}
	logResultBudget(env.Log, env.InvestigationID, "collect", len(views), outcome)
	return okResult(payload)
}

// buildTaskViews loads each task's compact projection (summary, hints,
// artifact names, log artifact index) from the store.
func buildTaskViews(ctx context.Context, env HandlerEnv, taskIDs []string) []taskView {
	out := make([]taskView, 0, len(taskIDs))
	for _, id := range taskIDs {
		task, err := getTask(ctx, env, id)
		if err != nil {
			out = append(out, taskView{TaskID: id, Status: "missing", Error: err.Error()})
			continue
		}
		v := taskView{TaskID: id, HostID: task.HostID, Collector: task.Collector, Status: task.Status}
		if task.DurationMs.Valid {
			v.DurationMS = task.DurationMs.Int64
		}
		if task.FinishedAt.Valid {
			v.CollectedAt = task.FinishedAt.Time.UTC().Format(time.RFC3339)
		}
		v.Error = task.Error
		if res, err := env.Store.GetResult(ctx, id); err == nil {
			v.Summary = compactDataSummary(res.DataJSON)
			var hints []any
			_ = json.Unmarshal(res.HintsJSON, &hints)
			v.Hints = hints
			v.Artifacts = listArtifactNames(res.ArtifactDir)
			v.ArtifactIndex = loadArtifactIndexes(res.ArtifactDir)
		}
		out = append(out, v)
	}
	return out
}

// batchRollup is the cross-host log summary returned for a collect_batch where
// every task that produced a log index ran the same collector. It replaces N
// repeated per-host TopPatterns blocks with one merged, severity-ranked cluster
// list; the per-host drill-in map (task_id + artifact names + sizes) stays in
// the tasks[] headlines so search_artifact / get_full_result still work.
type batchRollup struct {
	Collector       string                    `json:"collector"`
	HostCount       int                       `json:"host_count"`
	Clusters        []logtriage.RolledCluster `json:"clusters"`
	OmittedClusters int                       `json:"omitted_clusters,omitempty"`
	Hint            string                    `json:"_hint,omitempty"`
}

// rollupStats carries deterministic counters for verbose logging only.
type rollupStats struct {
	BatchSize      int
	HostsCovered   int
	ClustersBefore int
	ClustersAfter  int
}

// maybeBatchRollup returns a cross-host roll-up when at least two tasks produced
// a log artifact_index AND they all ran the same collector. The third return is
// whether roll-up applies; the fourth signals collectors differing across the
// indexed tasks (callers WARN and fall back to per-task indexes).
func maybeBatchRollup(views []taskView, maxHostsPerCluster int) (*batchRollup, rollupStats, bool, bool) {
	collector := ""
	differ := false
	perHost := make([]logtriage.HostClusters, 0, len(views))
	stats := rollupStats{BatchSize: len(views)}
	for _, v := range views {
		var clusters []logtriage.Cluster
		for _, idx := range v.ArtifactIndex {
			clusters = append(clusters, idx.TopPatterns...)
		}
		if len(clusters) == 0 {
			continue
		}
		if collector == "" {
			collector = v.Collector
		} else if v.Collector != collector {
			differ = true
		}
		stats.ClustersBefore += len(clusters)
		perHost = append(perHost, logtriage.HostClusters{HostID: v.HostID, TaskID: v.TaskID, Clusters: clusters})
	}
	if len(perHost) < 2 || differ {
		return nil, stats, false, differ
	}
	rolled := logtriage.RollupClusters(perHost, maxHostsPerCluster)
	stats.HostsCovered = len(perHost)
	stats.ClustersAfter = len(rolled)
	return &batchRollup{
		Collector: collector,
		HostCount: len(perHost),
		Clusters:  rolled,
		Hint:      "clusters rolled up across hosts by (template, severity); drill into a host via search_artifact(task_id, pattern) using the task_id/artifacts in tasks[]",
	}, stats, true, false
}

// summarizeWithRollup assembles a collect_batch result around a cross-host
// roll-up: the per-task entries are reduced to drill-in headlines (no repeated
// patterns) and the budget is split between the roll-up detail and the host
// headlines so the whole result stays within the token cap.
func summarizeWithRollup(env HandlerEnv, views []taskView, rollup *batchRollup, stats rollupStats, maxTok int) ToolResult {
	if maxTok <= 0 {
		maxTok = defaultMaxResultTokens
	}
	for i := range views {
		for j := range views[i].ArtifactIndex {
			views[i].ArtifactIndex[j] = stripPatterns(views[i].ArtifactIndex[j])
		}
	}
	_, beforeRollupBytes := resultTokens(map[string]any{"batch_rollup": rollup})
	trimRollup(rollup, maxTok/2)
	rollupTokens, afterRollupBytes := resultTokens(map[string]any{"batch_rollup": rollup})
	taskBudget := maxTok - rollupTokens
	if min := maxTok / 4; taskBudget < min {
		taskBudget = min
	}
	views, meta, outcome := applyResultBudget(views, taskBudget)
	payload := map[string]any{"tasks": views, "batch_rollup": rollup}
	for k, v := range meta {
		payload[k] = v
	}
	if env.Log != nil {
		env.Log.Debug("batch rollup",
			"investigation_id", env.InvestigationID,
			"collector", rollup.Collector,
			"batch_size", stats.BatchSize,
			"hosts_covered", stats.HostsCovered,
			"distinct_clusters_before", stats.ClustersBefore,
			"distinct_clusters_after", stats.ClustersAfter,
			"omitted_clusters", rollup.OmittedClusters,
			"bytes_saved", beforeRollupBytes-afterRollupBytes)
	}
	logResultBudget(env.Log, env.InvestigationID, "collect_batch", len(views), outcome)
	return okResult(payload)
}

// logResultBudget emits verbose token-economy diagnostics for one result. It
// never logs raw artifact bodies — only counts, token estimates, and the
// demotion ladder taken.
func logResultBudget(log *slog.Logger, invID, op string, taskCount int, outcome resultBudgetOutcome) {
	if log == nil {
		return
	}
	log.Debug("result budget",
		"investigation_id", invID,
		"operation", op,
		"estimated_tokens", outcome.EstimatedTokens,
		"max_result_tokens", outcome.MaxTokens,
		"demotion_steps", outcome.DemotionSteps,
		"omitted_tasks", outcome.OmittedTasks,
		"final_tokens", outcome.FinalTokens)
	if outcome.Demoted {
		log.Info("tool result demoted to fit token budget",
			"investigation_id", invID,
			"operation", op,
			"tasks", taskCount,
			"omitted_tasks", outcome.OmittedTasks,
			"bytes_saved", outcome.EstimatedBytes-outcome.FinalBytes,
			"final_tokens", outcome.FinalTokens)
	}
}

// compactDataSummary returns a budget-aware preview of result JSON: full data
// if small, else a synopsis with size and top-level keys. Investigator can
// always pull full via get_full_result.
func compactDataSummary(raw []byte) any {
	const maxInline = 1500 // bytes — roughly ≤500 tokens
	if len(raw) <= maxInline {
		var v any
		_ = json.Unmarshal(raw, &v)
		return v
	}
	var top any
	_ = json.Unmarshal(raw, &top)
	keys := topLevelKeys(top)
	return map[string]any{
		"_truncated":  true,
		"_size_bytes": len(raw),
		"_top_keys":   keys,
		"_hint":       "call get_full_result(task_id) for the complete object",
	}
}

func topLevelKeys(v any) []string {
	switch m := v.(type) {
	case map[string]any:
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	case []any:
		return []string{fmt.Sprintf("(array, len=%d)", len(m))}
	}
	return nil
}

// loadArtifactIndexes returns the per-file logtriage index for a task's
// artifact dir. It prefers the precomputed _index.json the runner wrote
// (Task 10); if that is missing or unreadable it computes the index on the
// fly (best-effort, bounded). Empty dir → nil.
func loadArtifactIndexes(dir string) []logtriage.ArtifactIndex {
	if dir == "" {
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(dir, logtriage.IndexFileName)); err == nil {
		var wrap struct {
			Artifacts []logtriage.ArtifactIndex `json:"artifacts"`
		}
		if json.Unmarshal(b, &wrap) == nil && len(wrap.Artifacts) > 0 {
			return wrap.Artifacts
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []logtriage.ArtifactIndex{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == logtriage.IndexFileName {
			continue
		}
		if idx, ierr := logtriage.IndexFile(filepath.Join(dir, e.Name())); ierr == nil {
			out = append(out, idx)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// listArtifactNames returns the on-disk (already-sanitized) names of a task's
// searchable artifacts. The runner's _index.json sidecar is excluded — it is
// the index, not a grep target, and surfacing it would let the model waste a
// search_artifact call on it.
func listArtifactNames(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || e.Name() == logtriage.IndexFileName {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

// resolveArtifactName maps an optional, model-supplied artifact_name to a real
// on-disk artifact for the task. An empty name resolves to the sole artifact;
// a wrong or ambiguous name yields an error that enumerates the valid names so
// the model never has to guess one — the original retrieval dead-end.
func resolveArtifactName(dir, requested string) (string, []string, error) {
	valid := listArtifactNames(dir)
	if len(valid) == 0 {
		return "", nil, fmt.Errorf("task has no artifacts to search")
	}
	if requested == "" {
		if len(valid) == 1 {
			return valid[0], valid, nil
		}
		return "", valid, fmt.Errorf("task has %d artifacts; pass artifact_name (one of: %s)", len(valid), strings.Join(valid, ", "))
	}
	for _, n := range valid {
		if n == requested {
			return requested, valid, nil
		}
	}
	return "", valid, fmt.Errorf("no artifact named %q for this task; valid names: %s", requested, strings.Join(valid, ", "))
}

func getTask(ctx context.Context, env HandlerEnv, id string) (store.Task, error) {
	return env.Store.GetTask(ctx, id)
}

func handleCollect(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	exec, err := PrepareCollect(ctx, env, argsJSON)
	if err != nil {
		return errResult(err)
	}
	return SummarizeTasks(ctx, env, exec.TaskIDs)
}

func handleCollectBatch(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	exec, err := PrepareCollectBatch(ctx, env, argsJSON)
	if err != nil {
		return errResult(err)
	}
	return SummarizeTasks(ctx, env, exec.TaskIDs)
}

func handleGetFullResult(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	var a struct {
		TaskID string `json:"task_id"`
		Offset int    `json:"offset"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return errResult(err)
	}
	if a.TaskID == "" {
		return errResult(fmt.Errorf("task_id required"))
	}
	res, err := env.Store.GetResult(ctx, a.TaskID)
	if err != nil {
		return errResult(err)
	}
	total := len(res.DataJSON)
	// Small result, first page: return the parsed structured data as before.
	if total <= getFullResultCap && a.Offset == 0 {
		var data any
		_ = json.Unmarshal(res.DataJSON, &data)
		return okResult(map[string]any{"task_id": a.TaskID, "data": data})
	}
	// Oversized (reached via force:true or the artifact-less allow in
	// preflightRetrieval) or an explicit paging request: return a BOUNDED raw
	// byte window so a huge structured result (e.g. a 716KB systemd_units with no
	// searchable artifact) cannot dump its whole body into the LLM context
	// (three-tier invariant). The window is raw JSON text — it may not be
	// independently parseable — with paging metadata so the model can fetch the
	// next slice via offset.
	if a.Offset < 0 || a.Offset > total {
		a.Offset = 0
	}
	end := a.Offset + getFullResultCap
	if end > total {
		end = total
	}
	out := map[string]any{
		"task_id":        a.TaskID,
		"truncated":      true,
		"total_bytes":    total,
		"offset":         a.Offset,
		"returned_bytes": end - a.Offset,
		"data_window":    string(res.DataJSON[a.Offset:end]),
	}
	if end < total {
		out["next_offset"] = end
		out["note"] = fmt.Sprintf(
			"Oversized result: showing bytes %d–%d of %d. Call get_full_result(task_id=%q, offset=%d) "+
				"for the next window. Prefer search_artifact when the task has an artifact.",
			a.Offset, end, total, a.TaskID, end)
	} else {
		out["note"] = fmt.Sprintf("Final window: bytes %d–%d of %d.", a.Offset, end, total)
	}
	return okResult(out)
}

// recallConclusion mirrors the structured mark_done summary fields recall_prior
// surfaces. A small local copy avoids importing the web package; unknown fields
// are ignored so it tolerates legacy / partial payloads.
type recallConclusion struct {
	RootCause              string   `json:"root_cause,omitempty"`
	RootCauseExplains      string   `json:"root_cause_explains,omitempty"`
	Confidence             string   `json:"confidence,omitempty"`
	Symptoms               []string `json:"symptoms,omitempty"`
	RecommendedRemediation string   `json:"recommended_remediation,omitempty"`
	WhereToLookNext        []string `json:"where_to_look_next,omitempty"`
	EvidenceRefs           []string `json:"evidence_refs,omitempty"`
}

func (c recallConclusion) empty() bool {
	return strings.TrimSpace(c.RootCause) == "" && strings.TrimSpace(c.RecommendedRemediation) == "" &&
		len(c.Symptoms) == 0 && len(c.WhereToLookNext) == 0
}

// priorConclusion extracts a prior's conclusion: the finalized mark_done summary
// for a done investigation, else the latest mark_done PROPOSAL for one that never
// closed (the user's "write me that config again" usually lives there). Returns
// (conclusion, source-label, ok).
func priorConclusion(ctx context.Context, env HandlerEnv, inv store.Investigation) (recallConclusion, string, bool) {
	if p, ok := store.ParseInvestigationTerminalPayload(inv.SummaryJSON); ok && len(p.Summary) > 0 {
		var c recallConclusion
		if json.Unmarshal(p.Summary, &c) == nil && !c.empty() {
			return c, "final conclusion (done)", true
		}
	}
	// Not finalized: surface the most recent mark_done proposal, if any.
	tcs, err := env.Store.ListToolCalls(ctx, inv.ID)
	if err != nil {
		return recallConclusion{}, "", false
	}
	for i := len(tcs) - 1; i >= 0; i-- {
		if tcs[i].Tool != "mark_done" {
			continue
		}
		var wrap struct {
			Summary recallConclusion `json:"summary"`
		}
		if json.Unmarshal([]byte(tcs[i].InputJSON), &wrap) == nil && !wrap.Summary.empty() {
			return wrap.Summary, "proposed conclusion (status " + inv.Status + ", not finalized)", true
		}
	}
	return recallConclusion{}, "", false
}

// handleRecallPrior returns the FULL recorded conclusion + untruncated findings
// of a prior investigation attached to this run, so the model can re-use earlier
// work instead of re-collecting from the host. Read-only; gated to AttachedPriors
// (no fishing arbitrary investigations) and bounded by the result token budget
// (three-tier invariant) by trimming findings from the tail.
func handleRecallPrior(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	var a struct {
		InvestigationID string `json:"investigation_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return errResult(err)
	}
	a.InvestigationID = strings.TrimSpace(a.InvestigationID)
	if a.InvestigationID == "" {
		return errResult(fmt.Errorf("investigation_id required"))
	}
	if !env.priorAttached(a.InvestigationID) {
		return errResult(fmt.Errorf(
			"investigation_id %q is not an attached prior of this investigation — only the priors listed under [CROSS_INVESTIGATION_HINT] are retrievable",
			a.InvestigationID))
	}
	if env.Store == nil {
		return errResult(fmt.Errorf("store unavailable"))
	}
	inv, err := env.Store.GetInvestigation(ctx, a.InvestigationID)
	if err != nil {
		return errResult(err)
	}

	hosts := inv.AllowedHosts
	if len(hosts) == 0 {
		hosts = []string{"all hosts"}
	}
	out := map[string]any{
		"investigation_id": inv.ID,
		"incident":         inv.Goal,
		"status":           inv.Status,
		"hosts":            hosts,
	}
	if !inv.CreatedAt.IsZero() {
		out["date"] = inv.CreatedAt.UTC().Format("2006-01-02")
	}
	if c, source, ok := priorConclusion(ctx, env, inv); ok {
		out["conclusion"] = c
		out["conclusion_source"] = source
	} else {
		out["conclusion_source"] = "no conclusion recorded yet — see findings"
	}

	// Active findings, untruncated, highest-severity/pinned first.
	type recallFinding struct {
		Severity     string   `json:"severity"`
		Code         string   `json:"code"`
		Message      string   `json:"message"`
		EvidenceRefs []string `json:"evidence_refs,omitempty"`
		Pinned       bool     `json:"pinned,omitempty"`
	}
	var findings []recallFinding
	if fs, ferr := env.Store.ListFindings(ctx, inv.ID); ferr == nil {
		for _, f := range topFindings(fs, -1) {
			findings = append(findings, recallFinding{
				Severity: f.Severity, Code: f.Code, Message: f.Message,
				EvidenceRefs: findingEvidenceRefs(f.EvidenceJSON), Pinned: f.Pinned,
			})
		}
	}
	out["findings"] = findings

	// Bound by the result budget (three-tier invariant). The conclusion is the
	// point of the call and kept whole; trim findings from the tail if oversized.
	maxTok := env.MaxResultTokens
	if maxTok <= 0 {
		maxTok = defaultMaxResultTokens
	}
	for len(findings) > 0 {
		if t, _ := resultTokens(out); t <= maxTok {
			break
		}
		findings = findings[:len(findings)-1]
		out["findings"] = findings
		out["_findings_truncated"] = true
		out["_hint"] = "some findings omitted to fit token budget — they remain on the prior's detail page"
	}
	out["evidence_note"] = "This is a SEPARATE earlier investigation. Re-verify against THIS run's evidence and cite only THIS run's task_ids in add_finding."
	return okResult(out)
}

// perlOnlyConstructRE flags lookahead/lookbehind ((?=,(?!,(?<…) and
// backreferences (\1) — Perl-regex features RE2 cannot compile — so
// search_artifact can return an actionable rewrite hint instead of a bare
// "unsupported Perl syntax".
var perlOnlyConstructRE = regexp.MustCompile(`\(\?[=!<]|\\[1-9]`)

func handleSearchArtifact(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	var a struct {
		TaskID       string `json:"task_id"`
		ArtifactName string `json:"artifact_name"`
		Pattern      string `json:"pattern"`
		ContextLines int    `json:"context_lines"`
		MaxMatches   int    `json:"max_matches"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return errResult(err)
	}
	if a.TaskID == "" || a.Pattern == "" {
		return errResult(fmt.Errorf("task_id and pattern required"))
	}
	// (C3) Cap pattern length and reject anchored quantifier-of-quantifier
	// shapes that even RE2 evaluates in O(n²) on large input. The list is
	// best-effort — we also enforce a per-line budget below.
	if len(a.Pattern) > 512 {
		return errResult(fmt.Errorf("pattern too long (%d bytes); max 512", len(a.Pattern)))
	}
	if a.MaxMatches <= 0 {
		a.MaxMatches = 50
	}
	if a.MaxMatches > 500 {
		a.MaxMatches = 500
	}
	if a.ContextLines < 0 {
		a.ContextLines = 0
	}
	if a.ContextLines > 20 {
		a.ContextLines = 20
	}

	res, err := env.Store.GetResult(ctx, a.TaskID)
	if err != nil {
		return errResult(err)
	}
	// Resolve the (optional) artifact_name to a real on-disk artifact; an
	// omitted name defaults to the task's sole artifact, a wrong name returns
	// an error that lists the valid names instead of a raw "no such file".
	name, _, rerr := resolveArtifactName(res.ArtifactDir, a.ArtifactName)
	if rerr != nil {
		return errResult(rerr)
	}
	a.ArtifactName = name
	clean := filepath.Clean(filepath.Join(res.ArtifactDir, a.ArtifactName))
	if !strings.HasPrefix(clean, filepath.Clean(res.ArtifactDir)+string(os.PathSeparator)) {
		return errResult(fmt.Errorf("path traversal"))
	}
	// (C3) Cap how much of the artifact we load into memory — search is
	// best-effort over the prefix when the file exceeds the cap.
	const artifactReadCap = 4 * 1024 * 1024
	f, err := os.Open(clean) //nolint:gosec // path validated above
	if err != nil {
		return errResult(err)
	}
	body, err := io.ReadAll(io.LimitReader(f, artifactReadCap+1))
	_ = f.Close()
	if err != nil {
		return errResult(err)
	}
	scanned := body
	scanTruncated := false
	if int64(len(scanned)) > artifactReadCap {
		scanned = scanned[:artifactReadCap]
		scanTruncated = true
	}
	// Binary artifacts (e.g. a NUL-containing dump) would be split on '\n' and
	// regex-matched as garbage. Sniff the head and refuse with an actionable
	// error rather than returning meaningless line matches.
	sniff := scanned
	if len(sniff) > 8192 {
		sniff = sniff[:8192]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return errResult(fmt.Errorf("artifact %q appears to be binary (NUL bytes present) — search_artifact only works on text logs", a.ArtifactName))
	}

	re, err := regexp.Compile("(?i)" + a.Pattern)
	if err != nil {
		// RE2 (Go's regexp) rejects Perl-only constructs the model often reaches
		// for. A bare "invalid or unsupported Perl syntax" gave no way forward
		// (inv_a00000000001 seq 7 used a lookahead and stalled), so name the
		// limitation and a concrete rewrite path.
		if perlOnlyConstructRE.MatchString(a.Pattern) {
			return errResult(fmt.Errorf(
				"regex %q rejected: %w. RE2 does NOT support lookahead/lookbehind "+
					"((?=…), (?!…), (?<=…), (?<!…)) or backreferences (\\1); rewrite without them — "+
					"match the surrounding text directly and discriminate in your reasoning, or run two "+
					"separate search_artifact passes and intersect the returned line refs",
				a.Pattern, err))
		}
		return errResult(fmt.Errorf("regex %q rejected: %w", a.Pattern, err))
	}

	// (C3) Hard deadline on the regex pass itself. Goroutine + cancel via
	// dedicated context — RE2 is linear in input but with a bad pattern
	// can still spend tens of seconds on a 4 MiB blob.
	type result struct {
		hits []searchMatch
		err  error
	}
	done := make(chan result, 1)
	scanCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() {
		lines := strings.Split(string(scanned), "\n")
		var hits []searchMatch
		for i, ln := range lines {
			if scanCtx.Err() != nil {
				done <- result{hits: hits, err: scanCtx.Err()}
				return
			}
			if !re.MatchString(ln) {
				continue
			}
			m := searchMatch{LineNo: i + 1, Text: ln}
			if a.ContextLines > 0 {
				lo := max0(i - a.ContextLines)
				hi := minInt(len(lines), i+a.ContextLines+1)
				m.Context = append([]string(nil), lines[lo:hi]...)
			}
			hits = append(hits, m)
			if len(hits) >= a.MaxMatches {
				break
			}
		}
		done <- result{hits: hits}
	}()

	r := <-done
	if r.err != nil {
		return errResult(fmt.Errorf("regex scan timeout (5s) — narrow the pattern: %w", r.err))
	}

	// (Task 5) Even within max_matches/context_lines, a wide search can return
	// tens of K tokens. Cap the total output by the same per-result token
	// budget used for collect results; line refs survive so the model can
	// re-search a specific region.
	matchTruncated := len(r.hits) >= a.MaxMatches
	fixed := map[string]any{
		"task_id":        a.TaskID,
		"artifact":       a.ArtifactName,
		"truncated":      matchTruncated,
		"file_truncated": scanTruncated,
		"scanned_bytes":  len(scanned),
	}
	foundMatches := len(r.hits)
	kept, omitted, droppedContext, steps := capSearchMatches(r.hits, fixed, env.MaxResultTokens)

	out := map[string]any{
		"task_id":        a.TaskID,
		"artifact":       a.ArtifactName,
		"matches":        kept,
		"count":          len(kept),
		"truncated":      matchTruncated,
		"file_truncated": scanTruncated,
		"scanned_bytes":  len(scanned),
	}
	if omitted > 0 {
		out["omitted_matches"] = omitted
	}
	if omitted > 0 || droppedContext {
		out["_hint"] = searchBudgetHint
	}
	if env.Log != nil {
		resultTok, _ := resultTokens(out)
		env.Log.Debug("search artifact budget",
			"investigation_id", env.InvestigationID,
			"task_id", a.TaskID, "artifact", a.ArtifactName,
			"matches_found", foundMatches, "matches_returned", len(kept),
			"omitted_matches", omitted, "dropped_context", droppedContext,
			"demotion_steps", steps,
			"result_tokens", resultTok, "budget", maxResultTokensOr(env.MaxResultTokens))
	}
	return okResult(out)
}

// maxResultTokensOr resolves the effective per-result token cap for logging:
// the configured value, or the compiled default when unset.
func maxResultTokensOr(configured int) int {
	if configured > 0 {
		return configured
	}
	return defaultMaxResultTokens
}

func handleCompareAcrossHosts(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	var a struct {
		TaskIDs []string `json:"task_ids"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return errResult(err)
	}
	if len(a.TaskIDs) < 2 {
		return errResult(fmt.Errorf("at least 2 task_ids"))
	}
	type perHost struct {
		TaskID string         `json:"task_id"`
		HostID string         `json:"host_id"`
		Data   map[string]any `json:"data"`
	}
	rows := make([]perHost, 0, len(a.TaskIDs))
	for _, id := range a.TaskIDs {
		t, err := getTask(ctx, env, id)
		if err != nil {
			return errResult(err)
		}
		res, err := env.Store.GetResult(ctx, id)
		if err != nil {
			return errResult(err)
		}
		var d map[string]any
		_ = json.Unmarshal(res.DataJSON, &d)
		rows = append(rows, perHost{TaskID: id, HostID: t.HostID, Data: d})
	}
	keys := map[string]bool{}
	for _, r := range rows {
		for k := range r.Data {
			keys[k] = true
		}
	}
	agree := map[string]any{}
	differ := map[string]map[string]any{}
	for k := range keys {
		first := rows[0].Data[k]
		same := true
		for _, r := range rows[1:] {
			if !jsonEqual(first, r.Data[k]) {
				same = false
				break
			}
		}
		if same {
			agree[k] = first
		} else {
			perField := map[string]any{}
			for _, r := range rows {
				perField[r.HostID] = r.Data[k]
			}
			differ[k] = perField
		}
	}
	return okResult(map[string]any{"agree": agree, "differ": differ, "task_ids": a.TaskIDs})
}

// ---- Investigation meta -------------------------------------------------

func handleAddFinding(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	var a struct {
		Severity     string   `json:"severity"`
		Code         string   `json:"code"`
		Message      string   `json:"message"`
		EvidenceRefs []string `json:"evidence_refs"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return errResult(err)
	}
	if len(a.EvidenceRefs) == 0 {
		return errResult(fmt.Errorf("evidence_refs must contain at least one task_id"))
	}
	switch a.Severity {
	case "info", "warn", "error":
	default:
		return errResult(fmt.Errorf("severity must be info|warn|error"))
	}
	// (H4) The model can hallucinate task_ids. Verify each one resolves to
	// a real task in this hub — without this, findings memo grows full of
	// references to nonexistent tasks and the audit chain breaks.
	for _, ref := range a.EvidenceRefs {
		if _, err := env.Store.GetTask(ctx, ref); err != nil {
			return errResult(fmt.Errorf("evidence_ref %q: %w", ref, err))
		}
	}
	id := newFindingID()
	body, _ := json.Marshal(map[string]any{"task_ids": a.EvidenceRefs})
	if err := env.Store.AddFinding(ctx, store.Finding{
		ID: id, InvestigationID: env.InvestigationID,
		Severity: a.Severity, Code: a.Code, Message: a.Message,
		EvidenceJSON: string(body),
	}); err != nil {
		return errResult(err)
	}
	env.Bus.Publish(env.InvestigationID, EventFindingAdded, map[string]any{
		"finding_id":    id,
		"severity":      a.Severity,
		"code":          a.Code,
		"message":       a.Message,
		"evidence_refs": a.EvidenceRefs,
	})
	return okResult(map[string]any{"finding_id": id})
}

func handleAskOperator(_ context.Context, _ HandlerEnv, argsJSON string) ToolResult {
	var a struct {
		Question string `json:"question"`
		Context  string `json:"context"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return errResult(err)
	}
	if a.Question == "" {
		return errResult(fmt.Errorf("question required"))
	}
	// The loop sets investigation status='waiting' on this tool call; here
	// we just echo back so the LLM has a tool message to read on resume.
	return okResult(map[string]any{
		"asked": a.Question, "operator_response_pending": true,
	})
}

func handleMarkDone(ctx context.Context, env HandlerEnv, argsJSON string) ToolResult {
	// The loop is responsible for finalizing the investigation row from this
	// payload. Here we validate the structure and apply the coverage backstop;
	// returning OK:false reuses the loop's existing rejected-close path
	// (loop.go: `if !result.OK { break }`), so a bounce leaves the investigation
	// active without finalizing and without a dangling tool_call.
	var a struct {
		Summary map[string]any `json:"summary"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return errResult(err)
	}
	// An honest "no cause found" close is explicitly allowed by the prompt
	// (rule 9, state "inconclusive"). Accept it via root_cause == "inconclusive"
	// rather than rejecting; only a genuinely empty/absent root_cause is an
	// error, and the message tells the model exactly how to close inconclusively.
	rc, _ := a.Summary["root_cause"].(string)
	if strings.TrimSpace(rc) == "" {
		return errResult(fmt.Errorf("summary.root_cause required: give the root-cause paragraph, or the literal string \"inconclusive\" when no cause was established"))
	}
	// Structured-conclusion validation (rule 14/15): force an explanation-shaped
	// close so a symptom-less, negative ("ruled X out") conclusion can't finalize.
	// The fields are validated here (not only in the JSON schema) because not
	// every OpenAI-compatible backend enforces `required`/`enum` on tool args.
	inconclusive := strings.EqualFold(strings.TrimSpace(rc), "inconclusive")
	conf := strings.ToLower(strings.TrimSpace(asString(a.Summary["confidence"])))
	if conf == "" && inconclusive {
		conf = "inconclusive" // be lenient: a literal "inconclusive" root_cause implies it
	}
	switch conf {
	case "confirmed", "likely", "speculative", "inconclusive":
	case "":
		return errResult(fmt.Errorf("summary.confidence required: one of confirmed|likely|speculative|inconclusive (a conclusion that only rules things out without explaining the primary symptom is \"inconclusive\")"))
	default:
		return errResult(fmt.Errorf("summary.confidence %q invalid: use one of confirmed|likely|speculative|inconclusive", conf))
	}
	if len(asStringSlice(a.Summary["symptoms"])) == 0 {
		return errResult(fmt.Errorf("summary.symptoms required: list at least one directly OBSERVED symptom (rule 15) — what was actually seen, not a mechanism word like \"hung\"/\"froze\""))
	}
	if conf != "inconclusive" && strings.TrimSpace(asString(a.Summary["root_cause_explains"])) == "" {
		return errResult(fmt.Errorf("summary.root_cause_explains required: name which listed symptom(s) the root_cause causally explains (rule 14/15). If the primary symptom is not explained, set confidence:\"inconclusive\" instead"))
	}
	if conf != "confirmed" && len(asStringSlice(a.Summary["where_to_look_next"])) == 0 {
		return errResult(fmt.Errorf("summary.where_to_look_next required when confidence is not \"confirmed\": list 1-4 hypotheses you could not verify, each naming the collector/artifact that would confirm or refute it"))
	}
	// (HF2) Hypothesis-coverage backstop for prompt rule 14: bounce the first
	// mark_done of an investigation that anchored on one log cluster while a
	// multi-cluster index was on offer. One-time and operator-overridable; see
	// evaluateCoverageGate.
	if v := evaluateCoverageGate(ctx, env); v.bounce {
		return errResult(fmt.Errorf("%s", v.message))
	}
	// Explanatory-adequacy backstop for rules 14/15: before a CONFIDENT close,
	// force one self-critique turn that the conclusion actually explains the
	// primary symptom and that no symptom-matching observation was silently
	// dropped (the inv_a00000000003 failure the breadth-only coverage gate
	// missed). One-time and operator-overridable; humble closes are exempt.
	// Runs AFTER the coverage gate so breadth is settled before synthesis.
	if v := evaluateExplanationGate(ctx, env, conf); v.bounce {
		return errResult(fmt.Errorf("%s", v.message))
	}
	return okResult(map[string]any{"finalized": true})
}

// ---- helpers ------------------------------------------------------------

func parseSelector(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return out
}

func matchSelector(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func max0(a int) int {
	if a < 0 {
		return 0
	}
	return a
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func newFindingID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "f_" + hex.EncodeToString(b[:])
}

// asString coerces an unmarshalled JSON value to a string ("" when not a
// string). Used to validate mark_done summary fields parsed into map[string]any.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// asStringSlice coerces an unmarshalled JSON value to []string, keeping only
// non-empty string elements. Accepts a JSON array of strings; anything else
// yields an empty slice.
func asStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
