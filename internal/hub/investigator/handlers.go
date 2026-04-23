package investigator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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
	// Bus, when non-nil, receives finding.added events fired from
	// handleAddFinding so remote API subscribers see the finding without
	// polling. Nil-safe — Bus.Publish itself handles the nil receiver.
	Bus *Bus
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
	HostID         string            `json:"host_id"`
	Collector      string            `json:"collector"`
	Params         map[string]string `json:"params"`
	TimeoutSeconds int32             `json:"timeout_seconds"`
}

type collectBatchArgs struct {
	HostIDs        []string          `json:"host_ids"`
	Collector      string            `json:"collector"`
	Params         map[string]string `json:"params"`
	TimeoutSeconds int32             `json:"timeout_seconds"`
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

// SummarizeTasks produces the compact JSON the LLM sees after a collect /
// collect_batch completes. Goal is ≤2K tokens (PROJECT.md §7.4).
func SummarizeTasks(ctx context.Context, env HandlerEnv, taskIDs []string) ToolResult {
	type taskView struct {
		TaskID     string   `json:"task_id"`
		HostID     string   `json:"host_id"`
		Collector  string   `json:"collector"`
		Status     string   `json:"status"`
		DurationMS int64    `json:"duration_ms,omitempty"`
		Error      string   `json:"error,omitempty"`
		Summary    any      `json:"summary,omitempty"`
		Hints      []any    `json:"hints,omitempty"`
		Artifacts  []string `json:"artifacts,omitempty"`
	}
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
		v.Error = task.Error
		if res, err := env.Store.GetResult(ctx, id); err == nil {
			v.Summary = compactDataSummary(res.DataJSON)
			var hints []any
			_ = json.Unmarshal(res.HintsJSON, &hints)
			v.Hints = hints
			v.Artifacts = listArtifactNames(res.ArtifactDir)
		}
		out = append(out, v)
	}
	return okResult(map[string]any{"tasks": out})
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
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
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
	var data any
	_ = json.Unmarshal(res.DataJSON, &data)
	return okResult(map[string]any{"task_id": a.TaskID, "data": data})
}

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
	if a.TaskID == "" || a.ArtifactName == "" || a.Pattern == "" {
		return errResult(fmt.Errorf("task_id, artifact_name, pattern required"))
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
	if res.ArtifactDir == "" {
		return errResult(fmt.Errorf("task has no artifacts"))
	}
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

	re, err := regexp.Compile("(?i)" + a.Pattern)
	if err != nil {
		return errResult(fmt.Errorf("regex: %w", err))
	}

	// (C3) Hard deadline on the regex pass itself. Goroutine + cancel via
	// dedicated context — RE2 is linear in input but with a bad pattern
	// can still spend tens of seconds on a 4 MiB blob.
	type match struct {
		LineNo  int      `json:"line"`
		Text    string   `json:"text"`
		Context []string `json:"context,omitempty"`
	}
	type result struct {
		hits []match
		err  error
	}
	done := make(chan result, 1)
	scanCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() {
		lines := strings.Split(string(scanned), "\n")
		var hits []match
		for i, ln := range lines {
			if scanCtx.Err() != nil {
				done <- result{hits: hits, err: scanCtx.Err()}
				return
			}
			if !re.MatchString(ln) {
				continue
			}
			m := match{LineNo: i + 1, Text: ln}
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
	return okResult(map[string]any{
		"task_id":        a.TaskID,
		"artifact":       a.ArtifactName,
		"matches":        r.hits,
		"count":          len(r.hits),
		"truncated":      len(r.hits) >= a.MaxMatches,
		"file_truncated": scanTruncated,
		"scanned_bytes":  len(scanned),
	})
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

func handleMarkDone(_ context.Context, _ HandlerEnv, argsJSON string) ToolResult {
	// The loop is responsible for finalizing the investigation row from this
	// payload. Here we just echo back validated structure.
	var a struct {
		Summary map[string]any `json:"summary"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return errResult(err)
	}
	if _, ok := a.Summary["root_cause"]; !ok {
		return errResult(fmt.Errorf("summary.root_cause required"))
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
