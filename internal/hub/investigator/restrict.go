package investigator

import (
	"context"
	"encoding/json"
	"fmt"

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
		if (args.Severity == "warn" || args.Severity == "error" || args.Severity == "critical") &&
			len(args.EvidenceRefs) >= 2 {
			return true
		}
		return false
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
