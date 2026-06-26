package investigator

import (
	"context"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// autonomousArmed / withinAutonomousBudget are the predicates the loop uses to
// decide whether a bounded autonomous burst may auto-approve one more probe.
func TestAutonomousBudgetPredicates(t *testing.T) {
	cases := []struct {
		name       string
		inv        store.Investigation
		wantArmed  bool
		wantWithin bool
	}{
		{"not armed", store.Investigation{}, false, true},
		{"armed steps, within", store.Investigation{AutoRunUntilSteps: 15, TotalToolCalls: 10}, true, true},
		{"armed steps, at target", store.Investigation{AutoRunUntilSteps: 15, TotalToolCalls: 15}, true, false},
		{"armed steps, over", store.Investigation{AutoRunUntilSteps: 15, TotalToolCalls: 20}, true, false},
		{"armed tokens, within", store.Investigation{AutoRunUntilTokens: 300_000, TotalPromptTokens: 100_000, TotalCompletionTokens: 50_000}, true, true},
		{"armed tokens, over", store.Investigation{AutoRunUntilTokens: 300_000, TotalPromptTokens: 250_000, TotalCompletionTokens: 60_000}, true, false},
		{"armed both, steps OK but tokens over", store.Investigation{AutoRunUntilSteps: 15, AutoRunUntilTokens: 300_000, TotalToolCalls: 1, TotalPromptTokens: 300_001}, true, false},
	}
	for _, c := range cases {
		if got := autonomousArmed(c.inv); got != c.wantArmed {
			t.Errorf("%s: autonomousArmed = %v, want %v", c.name, got, c.wantArmed)
		}
		if got := withinAutonomousBudget(c.inv); got != c.wantWithin {
			t.Errorf("%s: withinAutonomousBudget = %v, want %v", c.name, got, c.wantWithin)
		}
	}
}

// While armed and within the sub-budget, a probe tool is auto-approved with no
// per-step confirmation — the whole point of "run within a budget".
func TestShouldAutoApprove_AutonomousWithinBudgetApprovesProbe(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-auton-within"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st}
	in := store.Investigation{ID: inv, AutoRunUntilSteps: 15, TotalToolCalls: 5}
	if !l.shouldAutoApprove(ctx, in, "collect") {
		t.Fatal("armed + within sub-budget must auto-approve a probe")
	}
}

// Once the armed sub-budget is reached, probes are NOT auto-approved — the loop
// holds and step() pauses the burst for review on the next turn.
func TestShouldAutoApprove_AutonomousOverBudgetHoldsProbe(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-auton-over"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st}
	overSteps := store.Investigation{ID: inv, AutoRunUntilSteps: 15, TotalToolCalls: 15}
	if l.shouldAutoApprove(ctx, overSteps, "collect") {
		t.Fatal("armed + at step target must NOT auto-approve")
	}
	overTokens := store.Investigation{ID: inv, AutoRunUntilTokens: 300_000, TotalPromptTokens: 300_000}
	if l.shouldAutoApprove(ctx, overTokens, "collect") {
		t.Fatal("armed + at token target must NOT auto-approve")
	}
}

// The terminal mark_done review carve-out holds under autonomous mode too: a
// confident close surfaces for the operator unless OPERATOR FINALIZE was given.
func TestShouldAutoApprove_AutonomousTerminalHeldUnlessFinalize(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-auton-terminal"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st}
	in := store.Investigation{ID: inv, AutoRunUntilSteps: 15, TotalToolCalls: 5}
	if l.shouldAutoApprove(ctx, in, "mark_done") {
		t.Fatal("mark_done must be held under an armed autonomous run with no OPERATOR FINALIZE")
	}
	mustAppendUser(t, st, inv, "OPERATOR FINALIZE [priority: HIGH]\nEmit mark_done now.")
	if !l.shouldAutoApprove(ctx, in, "mark_done") {
		t.Fatal("OPERATOR FINALIZE must re-permit auto-approving mark_done under autonomous mode")
	}
}

// End-to-end: once the running totals reach the armed target, step() PAUSES the
// investigation for review and disarms the burst (so a re-arm starts fresh),
// emitting an AUTONOMOUS PAUSE note and never calling the LLM.
func TestStep_AutonomousBudgetPausesAndDisarms(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-auton-pause"
	mustInsertInv(t, st, inv)
	// Arm a 1-step burst, then push the tool-call total to the target. maxSteps is
	// high so the GLOBAL cap does not pre-empt the autonomous pause we're testing.
	if err := st.SetAutonomousRun(ctx, inv, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.IncrementToolCalls(ctx, inv); err != nil {
		t.Fatal(err)
	}
	l := &Loop{store: st, bus: NewBus(), maxSteps: 1000, maxTokens: 10_000_000}
	cont, err := l.step(ctx, inv)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if cont {
		t.Fatal("step must NOT continue after an autonomous pause")
	}
	got, err := st.GetInvestigation(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "paused" {
		t.Fatalf("status = %q, want paused", got.Status)
	}
	if got.AutoRunUntilSteps != 0 || got.AutoRunUntilTokens != 0 {
		t.Fatalf("burst must be disarmed on pause, got steps=%d tokens=%d", got.AutoRunUntilSteps, got.AutoRunUntilTokens)
	}
	msgs, err := st.ListMessages(ctx, inv, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "AUTONOMOUS PAUSE") {
			found = true
		}
	}
	if !found {
		t.Fatal("an AUTONOMOUS PAUSE system message must be appended on the burst pause")
	}
}

// armAutonomousRun is the headline mechanism of feature B: it converts the
// operator delta into ABSOLUTE totals targets AND grants the matching global
// budget headroom so the global-cap pause can't pre-empt the autonomous target.
// (Covers the StartAutonomousRun logic without the LLM gate / async spawn.)
func TestArmAutonomousRun_DeltaToAbsoluteAndBudgetGrant(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-arm-math"
	mustInsertInv(t, st, inv)
	// Existing usage so the absolute conversion is non-trivial: 3 calls, 100k tokens.
	for i := 0; i < 3; i++ {
		if err := st.IncrementToolCalls(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AccumulateTokens(ctx, inv, 70_000, 30_000); err != nil {
		t.Fatal(err)
	}
	l := &Loop{store: st, bus: NewBus()}
	if err := l.armAutonomousRun(ctx, inv, 15, 300_000, "op"); err != nil {
		t.Fatalf("arm: %v", err)
	}
	got, err := st.GetInvestigation(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRunUntilSteps != 3+15 {
		t.Fatalf("until_steps = %d, want current(3)+delta(15)=18", got.AutoRunUntilSteps)
	}
	if got.AutoRunUntilTokens != 100_000+300_000 {
		t.Fatalf("until_tokens = %d, want current(100k)+delta(300k)=400k", got.AutoRunUntilTokens)
	}
	if got.ExtraSteps != 15 || got.ExtraTokens != 300_000 {
		t.Fatalf("global budget must be granted the same delta, got extra steps=%d tokens=%d", got.ExtraSteps, got.ExtraTokens)
	}
	if got.Status != "active" {
		t.Fatalf("arming must (re)activate, got %q", got.Status)
	}
	msgs, _ := st.ListMessages(ctx, inv, true)
	armed := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "AUTONOMOUS RUN armed") {
			armed = true
		}
	}
	if !armed {
		t.Fatal("an 'AUTONOMOUS RUN armed' system message must be appended")
	}
}

func TestArmAutonomousRun_Rejects(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	l := &Loop{store: st, bus: NewBus()}
	const inv = "inv-arm-reject"
	mustInsertInv(t, st, inv)
	if err := l.armAutonomousRun(ctx, inv, 0, 0, "op"); err == nil {
		t.Fatal("0/0 budget must be rejected")
	}
	if err := st.FinishInvestigation(ctx, inv, "done", `{"kind":"done"}`); err != nil {
		t.Fatal(err)
	}
	if err := l.armAutonomousRun(ctx, inv, 5, 0, "op"); err == nil {
		t.Fatal("arming a done investigation must be rejected")
	}
}

// Regression for the global-cap/autonomous coincidence: when a burst target lands
// exactly on the global cap, the global-cap pause fires but MUST disarm the burst
// so the armed flag does not linger past the hard ceiling.
func TestStep_GlobalCapPauseDisarmsCoincidentBurst(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-globalcap-disarm"
	mustInsertInv(t, st, inv)
	if err := st.SetAutonomousRun(ctx, inv, 5, 0); err != nil { // armed target == global cap below
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := st.IncrementToolCalls(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	l := &Loop{store: st, bus: NewBus(), maxSteps: 5, maxTokens: 10_000_000} // cap==target==5
	cont, err := l.step(ctx, inv)
	if err != nil || cont {
		t.Fatalf("step: cont=%v err=%v (want false,nil)", cont, err)
	}
	got, _ := st.GetInvestigation(ctx, inv)
	if got.Status != "paused" {
		t.Fatalf("status=%q want paused", got.Status)
	}
	if got.AutoRunUntilSteps != 0 {
		t.Fatalf("global-cap pause must disarm the coincident burst, got until_steps=%d", got.AutoRunUntilSteps)
	}
}

// Regression for the swallowed-Approve bug: an already-approved tool call whose
// proposal pushed totals to the autonomous target MUST execute before the burst
// pauses — the autonomous-pause check sits AFTER the approved-call handling.
func TestStep_ApprovedCallExecutesBeforeAutonomousPause(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-approve-not-swallowed"
	mustInsertInv(t, st, inv)
	if err := st.SetAutonomousRun(ctx, inv, 1, 0); err != nil { // target 1
		t.Fatal(err)
	}
	if err := st.IncrementToolCalls(ctx, inv); err != nil { // at target
		t.Fatal(err)
	}
	// An operator-approved, field-complete inconclusive mark_done (inconclusive so
	// the explanation gate stays out of the way and it finalizes deterministically).
	md := `{"summary":{"root_cause":"inconclusive","confidence":"inconclusive","symptoms":["host unreachable over network"],"where_to_look_next":["switch logs"],"recommended_remediation":"none"}}`
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: "md1", InvestigationID: inv, Seq: 2, Tool: "mark_done", InputJSON: md, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	l := &Loop{store: st, bus: NewBus(), maxSteps: 1000, maxTokens: 10_000_000}
	if _, err := l.step(ctx, inv); err != nil {
		t.Fatalf("step: %v", err)
	}
	got, _ := st.GetInvestigation(ctx, inv)
	if got.Status == "paused" {
		t.Fatal("the operator-approved mark_done must execute, not be swallowed by the autonomous pause")
	}
	if got.Status != "done" {
		t.Fatalf("approved mark_done should have finalized the investigation, got status=%q", got.Status)
	}
}
