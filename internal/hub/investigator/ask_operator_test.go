package investigator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// Answering a pending ask_operator delivers the operator's text back to the
// model as that tool call's RESULT (so the model reads it as the answer) and
// resumes the loop — the gap the old echo-only handler left.
func TestAnswerOperator_DeliversAnswerAsToolResult(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-ask", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "q1", InvestigationID: "inv-ask", Seq: 1, Tool: "ask_operator",
		InputJSON: `{"question":"which node runs etcd?"}`, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)

	if err := l.AnswerOperator(ctx, "inv-ask", "q1", "etcd runs on node3", "operator"); err != nil {
		t.Fatalf("AnswerOperator: %v", err)
	}
	// spawn() re-enters the loop; the fake LLM parks a new ask_operator, so this
	// also confirms the loop resumed. Wait for it to settle before asserting.
	waitPending(t, st, "inv-ask")

	msgs, err := st.ListMessages(ctx, "inv-ask", true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID.Valid && m.ToolCallID.String == "q1" &&
			strings.Contains(m.Content, "etcd runs on node3") {
			found = true
		}
	}
	if !found {
		t.Fatal("the operator answer must be appended as the ask_operator tool result")
	}
}

// T9: a verbatim re-ask (same question after lowercase+trim) is blocked
// pre-execution; a genuinely different question proceeds.
func TestPreflightAskOperator_BlocksNearDuplicate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: "inv-dup", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: "q1", InvestigationID: "inv-dup", Seq: 1, Tool: "ask_operator", InputJSON: `{"question":"Which node runs etcd?"}`, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateToolCall(ctx, "q1", "executed", "auto", "", `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	l := &Loop{store: st}

	// Same question, different case + surrounding whitespace → blocked.
	dup := &store.ToolCallRow{ID: "q2", InvestigationID: "inv-dup", Tool: "ask_operator", InputJSON: `{"question":"  which node runs ETCD?  "}`}
	synth, blocked := l.preflightAskOperator(ctx, "inv-dup", dup)
	if !blocked {
		t.Fatal("a verbatim re-ask must be blocked")
	}
	if synth.OK || !strings.Contains(synth.Error, "already asked") {
		t.Fatalf("block must carry an actionable error: %+v", synth)
	}
	// A genuinely different question proceeds.
	diff := &store.ToolCallRow{ID: "q3", InvestigationID: "inv-dup", Tool: "ask_operator", InputJSON: `{"question":"which node runs the api server?"}`}
	if _, blocked := l.preflightAskOperator(ctx, "inv-dup", diff); blocked {
		t.Fatal("a different question must proceed")
	}
}

// T9: a blocked re-ask must NOT flip the investigation to 'waiting' — otherwise
// a suppressed near-duplicate strands the run waiting on an answer the model
// never actually asked for.
func TestExecuteApproved_BlockedAskOperatorDoesNotLeakWaiting(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: "inv-leak", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: "a1", InvestigationID: "inv-leak", Seq: 1, Tool: "ask_operator", InputJSON: `{"question":"what changed at 02:00?"}`, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateToolCall(ctx, "a1", "executed", "auto", "", `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: "a2", InvestigationID: "inv-leak", Seq: 2, Tool: "ask_operator", InputJSON: `{"question":"what changed at 02:00?"}`, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	l := &Loop{store: st, bus: NewBus()}
	a2 := store.ToolCallRow{ID: "a2", InvestigationID: "inv-leak", Tool: "ask_operator", InputJSON: `{"question":"what changed at 02:00?"}`, Status: "pending"}
	if err := l.executeApproved(ctx, "inv-leak", &a2); err != nil {
		t.Fatalf("executeApproved: %v", err)
	}

	inv, err := st.GetInvestigation(ctx, "inv-leak")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status == "waiting" {
		t.Fatal("a blocked re-ask leaked status=waiting")
	}
	// The re-ask was recorded executed with the block result (OK:false).
	tcs, _ := st.ListToolCalls(ctx, "inv-leak")
	var a2row *store.ToolCallRow
	for i := range tcs {
		if tcs[i].ID == "a2" {
			a2row = &tcs[i]
		}
	}
	if a2row == nil || a2row.Status != "executed" || !a2row.ResultJSON.Valid ||
		!strings.Contains(a2row.ResultJSON.String, "already asked") {
		t.Fatalf("blocked re-ask not recorded with the block result: %+v", a2row)
	}
}

// T9 review fix: a preflight-blocked re-ask is recorded executed with OK:false,
// but it was never put to the operator — askOperatorStreak must skip it so the
// anti-loop nudge counts only genuine asks.
func TestAskOperatorStreak_IgnoresBlockedReAsk(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: "inv-streak", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	mk := func(id string, seq int, resultJSON string) {
		if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: id, InvestigationID: "inv-streak", Seq: seq, Tool: "ask_operator", InputJSON: `{"question":"q"}`, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateToolCall(ctx, id, "executed", "auto", "", resultJSON); err != nil {
			t.Fatal(err)
		}
	}
	mk("g1", 1, `{"ok":true}`)                          // genuine ask
	mk("b1", 2, `{"ok":false,"error":"already asked"}`) // preflight-blocked re-ask
	mk("g2", 3, `{"ok":true}`)                          // genuine ask

	l := &Loop{store: st}
	if n := l.askOperatorStreak(ctx, "inv-streak"); n != 2 {
		t.Fatalf("streak = %d, want 2 (the blocked re-ask must not be counted)", n)
	}
}

// AnswerOperator only answers a pending ask_operator — not any other pending
// tool call.
func TestAnswerOperator_RejectsNonAskOperatorPending(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-c", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "c1", InvestigationID: "inv-c", Seq: 1, Tool: "collect",
		InputJSON: `{"host_id":"h"}`, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)
	if err := l.AnswerOperator(ctx, "inv-c", "c1", "irrelevant", "operator"); err == nil {
		t.Fatal("answering a non-ask_operator pending call must error")
	}
}
