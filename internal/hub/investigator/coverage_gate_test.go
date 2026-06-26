package investigator

import (
	"context"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// Stored collect tool-result shapes the coverage gate inspects for breadth.
const (
	// >= 2 clusters in a single per-task artifact_index → breadth available.
	multiClusterResult = `{"ok":true,"data":{"tasks":[{"artifact_index":[{"top_patterns":[{"template":"tpm_try_transmit"},{"template":"eno1 link down"}]}]}]}}`
	// exactly 1 cluster → nothing to differentiate against.
	singleClusterResult = `{"ok":true,"data":{"tasks":[{"artifact_index":[{"top_patterns":[{"template":"tpm_try_transmit"}]}]}]}}`
	// budget-collapsed index (headline only) still signals there WAS breadth.
	truncatedResult = `{"ok":true,"data":{"tasks":[{"_index_truncated":true,"artifact_index":[{"top_patterns":[{"template":"tpm_try_transmit"}]}]}]}}`
)

func mustInsertInv(t *testing.T, st *store.Store, inv string) {
	t.Helper()
	if err := st.InsertInvestigation(context.Background(), store.Investigation{
		ID: inv, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
}

// insertExecutedTCWithResult is insertExecutedTC with a caller-controlled
// stored tool result (the coverage gate reads collect/mark_done results).
func insertExecutedTCWithResult(t *testing.T, st *store.Store, inv, id, tool, input, result string, seq int) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: id, InvestigationID: inv, Seq: seq, Tool: tool, InputJSON: input, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateToolCall(ctx, id, "executed", "auto", "task-"+id, result); err != nil {
		t.Fatal(err)
	}
}

func gateEnv(st *store.Store, inv string) HandlerEnv {
	return HandlerEnv{Store: st, InvestigationID: inv}
}

// The motivating failure: a multi-cluster log index was surfaced but the model
// anchored on the loudest cluster (zero distinct drills) and tried to close.
func TestCoverageGate_BouncesAnchoredMarkDone(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-bounce"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, multiClusterResult, 1)
	insertExecutedTC(t, st, inv, "f1", "add_finding", `{"severity":"warn","code":"tpm","message":"m","evidence_refs":["t1","t2"]}`, 2)

	v := evaluateCoverageGate(ctx, gateEnv(st, inv))
	if !v.bounce {
		t.Fatal("anchored mark_done (multi-cluster index, no drills) must bounce")
	}
	if !strings.Contains(v.message, coverageNudgeMarker) {
		t.Fatalf("bounce message must carry the marker, got: %s", v.message)
	}
	if !strings.Contains(v.message, "rule 14") {
		t.Fatalf("bounce message must reference rule 14, got: %s", v.message)
	}
}

// A collapsed (_index_truncated) inline index still proves breadth existed —
// the rare cluster is recoverable only via get_full_result, so bouncing here is
// exactly the point of the gate.
func TestCoverageGate_BouncesOnTruncatedIndex(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-trunc"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, truncatedResult, 1)

	if v := evaluateCoverageGate(ctx, gateEnv(st, inv)); !v.bounce {
		t.Fatal("a budget-collapsed (_index_truncated) index must still count as breadth and bounce")
	}
}

// Two distinct drill-downs across the index = the model looked past the loudest
// cluster → no bounce.
func TestCoverageGate_SufficientDrillsAccepted(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-drilled"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, multiClusterResult, 1)
	insertExecutedTC(t, st, inv, "s1", "search_artifact", `{"task_id":"task-c1","artifact_name":"journal","pattern":"tpm"}`, 2)
	insertExecutedTC(t, st, inv, "s2", "search_artifact", `{"task_id":"task-c1","artifact_name":"journal","pattern":"link down|carrier"}`, 3)

	if v := evaluateCoverageGate(ctx, gateEnv(st, inv)); v.bounce {
		t.Fatal("two distinct drill-downs must satisfy the breadth gate (no bounce)")
	}
}

// Repeating the SAME drill twice is one distinct region, not two → still bounces.
func TestCoverageGate_RepeatedSameDrillStillBounces(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-repeat"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, multiClusterResult, 1)
	insertExecutedTC(t, st, inv, "s1", "search_artifact", `{"task_id":"task-c1","artifact_name":"journal","pattern":"tpm"}`, 2)
	insertExecutedTC(t, st, inv, "s2", "search_artifact", `{"task_id":"task-c1","artifact_name":"journal","pattern":"tpm"}`, 3)

	if v := evaluateCoverageGate(ctx, gateEnv(st, inv)); !v.bounce {
		t.Fatal("two searches of the identical region count as one drill → must still bounce")
	}
}

// No multi-cluster breadth was ever surfaced → nothing to differentiate, no bounce.
func TestCoverageGate_NoBreadthAccepted(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-nobreadth"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"system_info","host_id":"h1"}`, singleClusterResult, 1)

	if v := evaluateCoverageGate(ctx, gateEnv(st, inv)); v.bounce {
		t.Fatal("a single-cluster index offers no breadth to explore → must not bounce")
	}
}

// One-time guarantee: a prior mark_done already carrying the nudge marker means
// the gate has fired once; the next close is accepted unconditionally.
func TestCoverageGate_OneTimeThenAccept(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-onetime"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, multiClusterResult, 1)
	priorBounce := `{"ok":false,"error":"` + coverageNudgeMarker + `: re-plan over the remaining clusters"}`
	insertExecutedTCWithResult(t, st, inv, "m1", "mark_done", `{"summary":{"root_cause":"tpm"}}`, priorBounce, 2)

	if v := evaluateCoverageGate(ctx, gateEnv(st, inv)); v.bounce {
		t.Fatal("the gate must fire at most once per investigation (no infinite block)")
	}
}

// An explicit operator finalize/redirect outranks the differential backstop.
func TestCoverageGate_OperatorFinalizeAccepted(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-opfin"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, multiClusterResult, 1)
	if _, err := st.AppendMessage(ctx, store.Message{
		InvestigationID: inv, Role: "user",
		Content: "OPERATOR FINALIZE [priority: HIGH]\nBudget exhausted. Emit mark_done NOW.",
	}); err != nil {
		t.Fatal(err)
	}

	if v := evaluateCoverageGate(ctx, gateEnv(st, inv)); v.bounce {
		t.Fatal("an OPERATOR FINALIZE directive must stand the coverage gate down")
	}
}

// End-to-end: a coverage-bounced mark_done must NOT finalize the investigation,
// and the nudge must be delivered as the mark_done tool-result message (balanced
// function_call_output — no dangling call, the patch-2026-06-20-21.06 hazard).
func TestExecuteApproved_CoverageBounceDoesNotFinalize(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-e2e"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, multiClusterResult, 1)

	// Payload is field-complete (passes mark_done's structured-conclusion
	// validation) so the call reaches the coverage gate, which is what this test
	// exercises — the bounce here is the coverage backstop, not field validation.
	input := `{"summary":{"root_cause":"tpm storm","root_cause_explains":"console spam tpm error -62","confidence":"likely","symptoms":["console spam tpm error -62"],"where_to_look_next":["nic/link flap — journal_tail kernel previous_boot"],"recommended_remediation":"x","evidence_refs":["task-c1"]}}`
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: "m1", InvestigationID: inv, Seq: 2, Tool: "mark_done", InputJSON: input, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	l := &Loop{store: st, bus: NewBus()}
	tc := &store.ToolCallRow{ID: "m1", InvestigationID: inv, Tool: "mark_done", InputJSON: input}
	if err := l.executeApproved(ctx, inv, tc); err != nil {
		t.Fatalf("executeApproved: %v", err)
	}

	got, err := st.GetInvestigation(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "done" {
		t.Fatal("a coverage-bounced mark_done must not finalize the investigation")
	}
	msgs, err := st.ListMessages(ctx, inv, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, coverageNudgeMarker) {
			found = true
		}
	}
	if !found {
		t.Fatal("the bounce nudge must be delivered as the mark_done tool-result message")
	}
}

// restrict.go invariant: a coverage-bounced mark_done becomes the newest executed
// tool, so the post-finding probe lockdown lifts for the single re-plan turn —
// otherwise the nudge ("go cover the unchecked class") would be unactionable.
func TestPostFindingRestricted_LiftedAfterCoverageBounce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-pf-cov"
	mustInsertInv(t, st, inv)
	insertExecutedTC(t, st, inv, "f1", "add_finding", `{"severity":"warn","code":"tpm","message":"m","evidence_refs":["t1","t2"]}`, 1)

	l := &Loop{store: st}
	if !l.postFindingRestricted(ctx, inv) {
		t.Fatal("a load-bearing finding should restrict the tool list")
	}

	bounce := `{"ok":false,"error":"` + coverageNudgeMarker + `: re-plan"}`
	insertExecutedTCWithResult(t, st, inv, "m1", "mark_done", `{"summary":{"root_cause":"tpm"}}`, bounce, 2)
	if l.postFindingRestricted(ctx, inv) {
		t.Fatal("a coverage-bounced mark_done must lift the post-finding lockdown for the re-plan turn")
	}
}
