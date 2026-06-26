package investigator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// postFindingAllowedTools is the small set of tools the model may pick on
// the turn immediately after it logged a load-bearing add_finding (severity
// ≥ warn, ≥2 evidence_refs). Pushes the model to either close the
// investigation, ask for operator input, or pile on more evidence — never
// to start a fresh probe branch.
var postFindingAllowedTools = []string{"mark_done", "ask_operator", "add_finding"}

// postFindingRestricted reports whether the most recent executed tool_call
// was a load-bearing add_finding. Best-effort: errors / missing fields fall
// back to false (don't restrict the model).
//
// Coverage-gate interaction (HF2): when the coverage gate bounces a mark_done
// (handlers.go handleMarkDone -> OK:false), that mark_done is still recorded as
// the newest *executed* tool_call. The walk below then hits a non-add_finding
// tool first and returns false, so the probe tools are re-offered for the single
// re-plan turn and the gate's nudge ("go cover the unchecked class") is
// actionable rather than a dead end — without contradicting rule 9, which only
// locks down the turn immediately after the finding. The gate's one-time
// guarantee prevents a re-bounce, so this cannot loop. Pinned by
// TestPostFindingRestricted_LiftedAfterCoverageBounce.
func (l *Loop) postFindingRestricted(ctx context.Context, invID string) bool {
	tcs, err := l.store.ListToolCalls(ctx, invID)
	if err != nil || len(tcs) == 0 {
		return false
	}
	// Walk back from the newest until we hit something that was actually
	// executed. Pending / skipped / aborted rows don't count toward the
	// "what did the model just do" question.
	for i := len(tcs) - 1; i >= 0; i-- {
		t := tcs[i]
		if t.Status != "executed" {
			continue
		}
		if t.Tool != "add_finding" {
			return false
		}
		var args struct {
			Severity     string   `json:"severity"`
			EvidenceRefs []string `json:"evidence_refs"`
		}
		if err := json.Unmarshal([]byte(t.InputJSON), &args); err != nil {
			return false
		}
		if isLoadBearingSeverity(args.Severity, len(args.EvidenceRefs)) {
			// A MUST-level operator directive (OPERATOR RESUME / OPERATOR
			// HYPOTHESIS) issued AFTER this finding reopens the investigation:
			// the post-finding lockdown must NOT strip the collect / log_search
			// tools that directive needs, or the operator's «грепай» / hypothesis
			// is structurally unactionable (CLAUDE.md invariant 4).
			if l.operatorDirectiveAfter(ctx, invID, t.ID) {
				return false
			}
			return true
		}
		return false
	}
	return false
}

// operatorDirectiveAfter reports whether a MUST-level operator directive was
// appended to the message log after the given finding's tool result. Used to
// lift the post-finding probe lockdown once the operator has explicitly
// directed further investigation.
func (l *Loop) operatorDirectiveAfter(ctx context.Context, invID, findingTCID string) bool {
	msgs, err := l.store.ListMessages(ctx, invID, true)
	if err != nil {
		return false
	}
	findingIdx := -1
	for i, m := range msgs {
		if m.Role == "tool" && m.ToolCallID.Valid && m.ToolCallID.String == findingTCID {
			findingIdx = i
			break
		}
	}
	if findingIdx < 0 {
		return false
	}
	for _, m := range msgs[findingIdx+1:] {
		if m.Role == "user" &&
			(strings.Contains(m.Content, "OPERATOR HYPOTHESIS") || strings.Contains(m.Content, "OPERATOR RESUME")) {
			return true
		}
	}
	return false
}

// preflightCollect short-circuits a collect / collect_batch tool_call before
// it reaches the runner queue. Catches host-offline and outside-allowlist
// conditions and synthesises an immediate tool result so the model sees a
// fast, actionable error instead of waiting for the runner round-trip.
// preflightCollectEconomy in loop.go runs after this and handles cost-side
// gates (broad selectors, etc.).
func (l *Loop) preflightCollect(ctx context.Context, invID string, tc *store.ToolCallRow) (ToolResult, bool) {
	var args struct {
		HostID  string   `json:"host_id"`
		HostIDs []string `json:"host_ids"`
	}
	if err := json.Unmarshal([]byte(tc.InputJSON), &args); err != nil {
		return ToolResult{}, false
	}
	check := func(host string) (ToolResult, bool) {
		if host == "" {
			return ToolResult{}, false
		}
		if l.online != nil && !l.online(host) {
			return errResult(fmt.Errorf("host %q is offline — pick a different host or wait for it to reconnect", host)), true
		}
		// Investigation scope already enforced inside PrepareCollect; we
		// re-check here so the model gets a faster signal.
		inv, err := l.store.GetInvestigation(ctx, invID)
		if err == nil && len(inv.AllowedHosts) > 0 {
			ok := false
			for _, h := range inv.AllowedHosts {
				if h == host {
					ok = true
					break
				}
			}
			if !ok {
				return errResult(fmt.Errorf("host %q is outside this investigation's allowlist (%v)", host, inv.AllowedHosts)), true
			}
		}
		return ToolResult{}, false
	}
	if synth, blocked := check(args.HostID); blocked {
		return synth, true
	}
	for _, h := range args.HostIDs {
		if synth, blocked := check(h); blocked {
			return synth, true
		}
	}
	return ToolResult{}, false
}

// getFullResultCap is the data_json size above which get_full_result is
// gated (Task 12). Structured results below this pass through; larger ones
// must be reached via the artifact index + search_artifact, or an explicit
// force override once a targeted search could not answer the evidence gap.
const getFullResultCap = 200 * 1024

// preflightRetrieval enforces the retrieval-economy guardrails (Task 12) on
// get_full_result and search_artifact before they reach the handler:
//   - get_full_result on an oversized result is blocked with a synthetic
//     result that steers the model to search_artifact / the artifact index,
//     unless the call carries "force": true.
//   - search_artifact repeating the identical (task_id, artifact, pattern)
//     a third time is blocked.
//
// Returns (synthetic ToolResult, true) when blocked; (_, false) to proceed.
func (l *Loop) preflightRetrieval(ctx context.Context, invID string, tc *store.ToolCallRow) (ToolResult, bool) {
	switch tc.Tool {
	case "get_full_result":
		var a struct {
			TaskID string `json:"task_id"`
			Force  bool   `json:"force"`
		}
		if err := json.Unmarshal([]byte(tc.InputJSON), &a); err != nil || a.TaskID == "" || a.Force {
			return ToolResult{}, false
		}
		res, err := l.store.GetResult(ctx, a.TaskID)
		if err != nil || len(res.DataJSON) <= getFullResultCap {
			return ToolResult{}, false
		}
		// Name the task's actual artifacts so the model never has to guess one
		// (that guessing is exactly what dead-ended inv_a00000000006).
		names := listArtifactNames(res.ArtifactDir)
		if len(names) == 0 {
			// Artifact-less oversized STRUCTURED result (e.g. systemd_units, 716KB
			// in inv_a00000000001 seq 11→14): search_artifact would dead-end at
			// resolveArtifactName ("task has no artifacts to search"), so blocking
			// here is the wedge. Proceed to handleGetFullResult, which now returns a
			// bounded, pageable window (offset) instead of the whole body — the
			// three-tier invariant is preserved by the handler, not by this block.
			if l.log != nil {
				l.log.Info("retrieval guardrail allow",
					"tool_call_id", tc.ID, "tool", tc.Tool, "task_id", a.TaskID,
					"reason", "oversized_artifactless_full_result", "size_bytes", len(res.DataJSON))
			}
			return ToolResult{}, false
		}
		if l.log != nil {
			l.log.Info("retrieval guardrail block",
				"tool_call_id", tc.ID, "tool", tc.Tool, "task_id", a.TaskID,
				"reason", "oversized_full_result", "size_bytes", len(res.DataJSON),
				"suggested", "search_artifact or get_full_result force:true")
		}
		return errResult(fmt.Errorf(
			"get_full_result blocked: result for %s is %d bytes (cap %d). Use "+
				"search_artifact(task_id=%q, artifact_name=%q, pattern=\"…\") — it returns line refs. "+
				"Available artifacts: %s. If a targeted search truly cannot answer the gap, retry "+
				"get_full_result with \"force\": true",
			a.TaskID, len(res.DataJSON), getFullResultCap, a.TaskID, names[0], strings.Join(names, ", "))), true

	case "search_artifact":
		var a struct {
			TaskID       string `json:"task_id"`
			ArtifactName string `json:"artifact_name"`
			Pattern      string `json:"pattern"`
		}
		if err := json.Unmarshal([]byte(tc.InputJSON), &a); err != nil {
			return ToolResult{}, false
		}
		// Normalize the (optional) artifact_name to the same canonical name the
		// handler resolves it to, so an omitted name and the explicitly-typed
		// default produce the SAME signature — otherwise the repeat-cap is
		// trivially evaded and the search loop this guard kills can recur.
		defaultName := ""
		if res, err := l.store.GetResult(ctx, a.TaskID); err == nil {
			if dn, _, derr := resolveArtifactName(res.ArtifactDir, ""); derr == nil {
				defaultName = dn
			}
		}
		norm := func(taskID, name string) string {
			if name == "" && taskID == a.TaskID {
				return defaultName
			}
			return name
		}
		sig := a.TaskID + "|" + norm(a.TaskID, a.ArtifactName) + "|" + a.Pattern
		tcs, err := l.store.ListToolCalls(ctx, invID)
		if err != nil {
			return ToolResult{}, false
		}
		n := 0
		for _, h := range tcs {
			if h.ID == tc.ID || h.Tool != "search_artifact" || h.Status != "executed" {
				continue
			}
			var b struct {
				TaskID       string `json:"task_id"`
				ArtifactName string `json:"artifact_name"`
				Pattern      string `json:"pattern"`
			}
			if json.Unmarshal([]byte(h.InputJSON), &b) != nil {
				continue
			}
			if b.TaskID+"|"+norm(b.TaskID, b.ArtifactName)+"|"+b.Pattern == sig {
				n++
			}
		}
		if n >= 2 {
			if l.log != nil {
				l.log.Info("retrieval guardrail block",
					"tool_call_id", tc.ID, "tool", tc.Tool, "task_id", a.TaskID,
					"artifact", a.ArtifactName, "reason", "search_repeat_cap", "prior_count", n)
			}
			return errResult(fmt.Errorf(
				"search_artifact blocked: the same (task_id, artifact, pattern) has already run %d times. "+
					"Change the pattern, widen context_lines, or call get_full_result — do not repeat the "+
					"identical search", n)), true
		}
	}
	return ToolResult{}, false
}

// filterTools narrows the offered tool catalog to the named subset. Order
// preserved; missing names silently ignored.
func filterTools(offered []llm.Tool, allow []string) []llm.Tool {
	want := make(map[string]struct{}, len(allow))
	for _, n := range allow {
		want[n] = struct{}{}
	}
	out := offered[:0:0]
	for _, t := range offered {
		if _, ok := want[t.Function.Name]; ok {
			out = append(out, t)
		}
	}
	return out
}
