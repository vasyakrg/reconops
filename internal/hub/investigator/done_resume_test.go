package investigator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// newDoneInvestigation seeds a COMPLETED investigation: system + user history
// plus a terminal done payload carrying a root_cause in summary_json (the shape
// finishTerminal writes on an accepted mark_done).
func newDoneInvestigation(t *testing.T, st *store.Store, id, rootCause string) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: id, Goal: "diagnose", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: id, Role: "system", Content: "system prompt"})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: id, Role: "user", Content: "etcd flapping"})
	// finishTerminal passes the INNER summary object (top-level root_cause) to
	// TerminalDonePayload — see loop.go mark_done finalize.
	summary := `{"root_cause":"` + rootCause + `","recommended_remediation":"restart the unit"}`
	if err := st.FinishInvestigation(ctx, id, "done", store.TerminalDonePayload(summary, time.Time{}).JSON()); err != nil {
		t.Fatal(err)
	}
}

// The operator-chosen behavior: a DONE investigation is reopened IN PLACE
// (done -> active), the stale terminal summary is cleared, and the prior
// conclusion is preserved in the OPERATOR RESUME directive so the model extends
// it rather than re-walking the same path.
func TestResumeAborted_ReopensDoneInPlaceWithPriorConclusion(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	newDoneInvestigation(t, st, "inv-done", "tpm rng storm starved the entropy pool")

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)

	if err := l.ResumeAborted(ctx, "inv-done", "the fix did not hold, re-check the renewal timer", "operator"); err != nil {
		t.Fatalf("ResumeAborted on a done investigation: %v", err)
	}
	// spawn() re-enters the loop; the fake LLM returns ask_operator → parks
	// pending, so the read below is race-free and proves it did NOT re-finalize.
	waitPending(t, st, "inv-done")

	got, err := st.GetInvestigation(ctx, "inv-done")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "done" {
		t.Fatalf("a reopened done investigation must not stay/return to done, got %q", got.Status)
	}
	if got.SummaryJSON.Valid {
		t.Fatalf("reopen must clear the stale terminal summary, got %q", got.SummaryJSON.String)
	}
	msgs, err := st.ListMessages(ctx, "inv-done", true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "user" &&
			strings.Contains(m.Content, "OPERATOR RESUME") &&
			strings.Contains(m.Content, "previously COMPLETED") &&
			strings.Contains(m.Content, "tpm rng storm") && // prior conclusion preserved
			strings.Contains(m.Content, "re-check the renewal timer") { // operator's new message
			found = true
		}
	}
	if !found {
		t.Fatal("reopened done must inject OPERATOR RESUME carrying the prior conclusion AND the operator message")
	}
}

// A non-reopenable status (active) must be a no-op: the claim loses, no
// OPERATOR RESUME is appended, and the status is unchanged.
func TestResumeAborted_RefusesNonReopenable(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-active", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)

	if err := l.ResumeAborted(ctx, "inv-active", "do something", "operator"); err != nil {
		t.Fatal(err)
	}
	inv, _ := st.GetInvestigation(ctx, "inv-active")
	if inv.Status != "active" {
		t.Fatalf("resume on an active investigation must be a no-op, got status %q", inv.Status)
	}
	msgs, _ := st.ListMessages(ctx, "inv-active", true)
	for _, m := range msgs {
		if strings.Contains(m.Content, "OPERATOR RESUME") {
			t.Fatal("a refused resume must not append an OPERATOR RESUME directive")
		}
	}
}
