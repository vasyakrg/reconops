package investigator

import (
	"context"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// Bug: clicking "Approve & close" did not close the investigation — the model-
// facing coverage/explanation backstops bounced the operator's explicit approval
// back to the LLM (OK:false), so executeApproved's `if !result.OK { break }`
// left the run active and the loop re-queried the model ("approve & close
// doesn't close, it loops"). An explicit operator approval is the authoritative
// close (CLAUDE.md invariant 4); the gates must stand down for it, exactly as
// they already do for OPERATOR FINALIZE.

func TestCoverageGate_OperatorApprovedCloseStandsDown(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-cov-op-approve"
	mustInsertInv(t, st, inv)
	// Multi-cluster breadth on offer, zero drills → the gate WOULD bounce.
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, multiClusterResult, 1)

	if v := evaluateCoverageGate(ctx, gateEnv(st, inv)); !v.bounce {
		t.Fatal("precondition: an anchored close must bounce without operator approval")
	}
	env := gateEnv(st, inv)
	env.OperatorApprovedClose = true
	if v := evaluateCoverageGate(ctx, env); v.bounce {
		t.Fatal("an explicit operator-approved close must stand the coverage gate down")
	}
}

func TestExplanationGate_OperatorApprovedCloseStandsDown(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-exp-op-approve"
	mustInsertInv(t, st, inv)

	// A confident close with no prior self-critique bounces by default.
	if v := evaluateExplanationGate(ctx, gateEnv(st, inv), "likely"); !v.bounce {
		t.Fatal("precondition: a confident close must bounce without operator approval")
	}
	env := gateEnv(st, inv)
	env.OperatorApprovedClose = true
	if v := evaluateExplanationGate(ctx, env, "likely"); v.bounce {
		t.Fatal("an explicit operator-approved close must stand the explanation gate down")
	}
}

// End-to-end mirror of TestExecuteApproved_CoverageBounceDoesNotFinalize: the
// SAME would-bounce scenario, but the mark_done is decided_by="operator" (the
// real "Approve & close" path) — it MUST finalize, not loop.
func TestExecuteApproved_OperatorApprovedCloseFinalizes(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-exec-op-approve"
	mustInsertInv(t, st, inv)
	insertExecutedTCWithResult(t, st, inv, "c1", "collect", `{"collector":"journal_tail","host_id":"h1"}`, multiClusterResult, 1)

	// Field-complete payload: passes structural validation, so the only thing
	// that could bounce it is the coverage gate — which must defer here.
	input := `{"summary":{"root_cause":"tpm storm","root_cause_explains":"console spam tpm error -62","confidence":"likely","symptoms":["console spam tpm error -62"],"where_to_look_next":["nic/link flap — journal_tail kernel previous_boot"],"recommended_remediation":"replace dimm","evidence_refs":["task-c1"]}}`
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: "m1", InvestigationID: inv, Seq: 2, Tool: "mark_done", InputJSON: input, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	// The operator clicks "Approve & close": DecideWithEdit stores decided_by="operator".
	if err := st.UpdateToolCall(ctx, "m1", "approved", "operator", "", ""); err != nil {
		t.Fatal(err)
	}

	l := &Loop{store: st, bus: NewBus(), nb: NewNotebook(t.TempDir(), nil)}
	tc, err := l.lastApproved(ctx, inv) // carries decided_by="operator"
	if err != nil || tc == nil {
		t.Fatalf("lastApproved: err=%v tc=%v", err, tc)
	}
	if tc.DecidedBy.String != "operator" {
		t.Fatalf("precondition: row must carry decided_by=operator, got %q", tc.DecidedBy.String)
	}
	if err := l.executeApproved(ctx, inv, tc); err != nil {
		t.Fatalf("executeApproved: %v", err)
	}

	got, err := st.GetInvestigation(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Fatalf("operator-approved close must finalize despite the coverage gate; status=%q", got.Status)
	}
}
