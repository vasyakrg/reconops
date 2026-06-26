package investigator

import (
	"context"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

func mustAppendUser(t *testing.T, st *store.Store, inv, content string) {
	t.Helper()
	if _, err := st.AppendMessage(context.Background(), store.Message{
		InvestigationID: inv, Role: "user", Content: content,
	}); err != nil {
		t.Fatal(err)
	}
}

// A terminal mark_done must NOT be auto-closed solely because AutoApprove is on
// — that is the "investigation marked done out of nowhere" bug. It is held for
// an explicit operator confirmation instead.
func TestShouldAutoApprove_TerminalHeldUnderAutoApprove(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-aa-terminal"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st}
	if l.shouldAutoApprove(ctx, store.Investigation{ID: inv, AutoApprove: true}, "mark_done") {
		t.Fatal("mark_done must be held pending under AutoApprove with no OPERATOR FINALIZE")
	}
}

// OPERATOR FINALIZE is an explicit close directive (CLAUDE.md invariant 4); it
// re-permits auto-approving the resulting mark_done so a hands-off finalize
// still closes the case in one shot.
func TestShouldAutoApprove_FinalizeRePermitsTerminal(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-aa-finalize"
	mustInsertInv(t, st, inv)
	mustAppendUser(t, st, inv, "OPERATOR FINALIZE [priority: HIGH]\nBudget exhausted. Emit mark_done NOW.")
	l := &Loop{store: st}
	if !l.shouldAutoApprove(ctx, store.Investigation{ID: inv, AutoApprove: true}, "mark_done") {
		t.Fatal("OPERATOR FINALIZE must re-permit auto-approving mark_done")
	}
}

// RESUME means "keep going", not "close now": a freshly resumed investigation
// must NOT auto-close on the model's next mark_done, or the reopen feature would
// reintroduce the exact spurious-done symptom.
func TestShouldAutoApprove_ResumeDoesNotRePermitTerminal(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-aa-resume"
	mustInsertInv(t, st, inv)
	mustAppendUser(t, st, inv, "OPERATOR RESUME [priority: HIGH]\nKeep investigating the NIC clusters.")
	l := &Loop{store: st}
	if l.shouldAutoApprove(ctx, store.Investigation{ID: inv, AutoApprove: true}, "mark_done") {
		t.Fatal("OPERATOR RESUME must NOT auto-close; mark_done stays pending")
	}
}

// The carve-out is surgical: cheap operator-gated probes still auto-approve
// under AutoApprove so hands-off discovery is unaffected.
func TestShouldAutoApprove_ProbeStillAutoApproved(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-aa-probe"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st}
	if !l.shouldAutoApprove(ctx, store.Investigation{ID: inv, AutoApprove: true}, "collect") {
		t.Fatal("a non-terminal tool must still auto-approve under AutoApprove")
	}
}

// Without AutoApprove, every operator-gated tool (including mark_done) waits;
// auto-tools are always pre-approved regardless of the toggle.
func TestShouldAutoApprove_NoAutoApproveHoldsGatedTools(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-aa-off"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st}
	if l.shouldAutoApprove(ctx, store.Investigation{ID: inv, AutoApprove: false}, "mark_done") {
		t.Fatal("mark_done must wait when AutoApprove is off")
	}
	if !l.shouldAutoApprove(ctx, store.Investigation{ID: inv, AutoApprove: false}, "list_hosts") {
		t.Fatal("list_hosts is an auto-tool and must always auto-approve")
	}
}
