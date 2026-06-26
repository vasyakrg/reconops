package investigator

import (
	"context"
	"testing"
)

// Regression guard for the operator-reported bleed: "enabling automation in one
// investigation made a DIFFERENT investigation run a host probe with no click,
// while that other investigation still showed automation off."
//
// Automation state (auto_approve, auto_run_until_*) and its global-budget grant
// are per-investigation rows. This test arms investigation A through the real
// production paths (armAutonomousRun → SetAutonomousRun + ExtendBudget, and the
// SetAutoApprove toggle) and asserts that investigation B's row is byte-for-byte
// unchanged AND that B's auto-approve decision still gates a probe. If any write
// path ever loses its WHERE id (the classic mass-update bug) or any decision
// reads shared/process state instead of the passed row, B would start
// auto-approving collect and this test fails.
func TestAutomation_DoesNotBleedAcrossInvestigations(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const (
		invA = "inv-bleed-a"
		invB = "inv-bleed-b"
	)
	mustInsertInv(t, st, invA)
	mustInsertInv(t, st, invB)

	// Give B real usage so an accidental delta-to-absolute write targeting B
	// would be detectable, and so its baseline is non-zero where it should be.
	for i := 0; i < 2; i++ {
		if err := st.IncrementToolCalls(ctx, invB); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AccumulateTokens(ctx, invB, 40_000, 10_000); err != nil {
		t.Fatal(err)
	}

	l := &Loop{store: st, bus: NewBus()}

	// Arm an autonomous burst on A only, and flip A's unbounded auto-approve.
	if err := l.armAutonomousRun(ctx, invA, 15, 300_000, "op"); err != nil {
		t.Fatalf("arm A: %v", err)
	}
	if err := st.SetAutoApprove(ctx, invA, true); err != nil {
		t.Fatalf("auto-approve A: %v", err)
	}

	// A must be armed — proves the writes actually happened (so B staying clean
	// is meaningful, not a no-op).
	a, err := st.GetInvestigation(ctx, invA)
	if err != nil {
		t.Fatal(err)
	}
	if !a.AutoApprove || a.AutoRunUntilSteps == 0 || a.AutoRunUntilTokens == 0 {
		t.Fatalf("A should be fully armed, got auto=%v steps=%d tokens=%d",
			a.AutoApprove, a.AutoRunUntilSteps, a.AutoRunUntilTokens)
	}

	// B must be completely untouched by arming A.
	b, err := st.GetInvestigation(ctx, invB)
	if err != nil {
		t.Fatal(err)
	}
	if b.AutoApprove {
		t.Error("BLEED: B.AutoApprove flipped on by arming A")
	}
	if b.AutoRunUntilSteps != 0 || b.AutoRunUntilTokens != 0 {
		t.Errorf("BLEED: B armed by arming A, got until_steps=%d until_tokens=%d", b.AutoRunUntilSteps, b.AutoRunUntilTokens)
	}
	if b.ExtraSteps != 0 || b.ExtraTokens != 0 {
		t.Errorf("BLEED: B's global budget grant changed by arming A, got extra_steps=%d extra_tokens=%d", b.ExtraSteps, b.ExtraTokens)
	}
	// B's own usage must be intact (no cross-write clobbered it).
	if b.TotalToolCalls != 2 {
		t.Errorf("B.TotalToolCalls = %d, want 2 (untouched)", b.TotalToolCalls)
	}

	// The decisive assertion: a probe proposed in B is NOT auto-approved — it
	// would be persisted pending for the operator, never executed.
	if l.shouldAutoApprove(ctx, b, "collect") {
		t.Fatal("BLEED: B auto-approved a collect probe while only A's automation was enabled")
	}
}

// Symmetric guard for the unbounded auto-approve toggle in isolation: enabling
// it on A alone must not make B auto-approve a probe.
func TestAutoApprove_TogglesPerInvestigationOnly(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const (
		invA = "inv-toggle-a"
		invB = "inv-toggle-b"
	)
	mustInsertInv(t, st, invA)
	mustInsertInv(t, st, invB)

	if err := st.SetAutoApprove(ctx, invA, true); err != nil {
		t.Fatal(err)
	}

	a, _ := st.GetInvestigation(ctx, invA)
	b, _ := st.GetInvestigation(ctx, invB)
	if !a.AutoApprove {
		t.Fatal("A.AutoApprove should be on")
	}
	if b.AutoApprove {
		t.Fatal("BLEED: B.AutoApprove turned on by toggling A")
	}

	l := &Loop{store: st}
	if l.shouldAutoApprove(ctx, b, "collect") {
		t.Fatal("BLEED: B auto-approved collect while only A's toggle was on")
	}
}
