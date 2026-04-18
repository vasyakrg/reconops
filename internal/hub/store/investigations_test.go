package store

import (
	"context"
	"testing"
)

func TestInvestigationLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	inv := Investigation{
		ID: "inv-1", Goal: "diagnose etcd", Status: "active",
		CreatedBy: "operator", Model: "anthropic/claude-sonnet-4.5",
		BaseURL: "https://openrouter.ai/api/v1",
	}
	if err := s.InsertInvestigation(ctx, inv); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetInvestigation(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "diagnose etcd" || got.Status != "active" {
		t.Fatalf("got %+v", got)
	}

	// Append messages, seq monotonic.
	for i := 0; i < 3; i++ {
		if _, err := s.AppendMessage(ctx, Message{
			InvestigationID: "inv-1", Role: "assistant", Content: "step",
		}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.ListMessages(ctx, "inv-1", false)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("messages: %v len=%d", err, len(msgs))
	}
	if msgs[0].Seq != 1 || msgs[2].Seq != 3 {
		t.Fatalf("seq broken: %+v", msgs)
	}

	// Tool call lifecycle.
	if err := s.InsertToolCall(ctx, ToolCallRow{
		ID: "call-1", InvestigationID: "inv-1", Seq: 1,
		Tool: "list_hosts", InputJSON: `{}`, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingToolCall(ctx, "inv-1")
	if err != nil || pending == nil || pending.ID != "call-1" {
		t.Fatalf("pending: %v %+v", err, pending)
	}
	if err := s.UpdateToolCall(ctx, "call-1", "executed", "operator", "task_xyz", `{"hosts":3}`); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.PendingToolCall(ctx, "inv-1"); pending != nil {
		t.Fatalf("still pending: %+v", pending)
	}

	// Finding.
	if err := s.AddFinding(ctx, Finding{
		ID: "f-1", InvestigationID: "inv-1",
		Severity: "warn", Code: "etcd.cert_near_expiry",
		Message: "kube-apiserver cert expires in 7 days",
	}); err != nil {
		t.Fatal(err)
	}
	fs, _ := s.ListFindings(ctx, "inv-1")
	if len(fs) != 1 || fs[0].Code != "etcd.cert_near_expiry" {
		t.Fatalf("findings: %+v", fs)
	}

	// Tokens accumulator.
	if err := s.AccumulateTokens(ctx, "inv-1", 1000, 200); err != nil {
		t.Fatal(err)
	}
	if err := s.IncrementToolCalls(ctx, "inv-1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetInvestigation(ctx, "inv-1")
	if got.TotalPromptTokens != 1000 || got.TotalCompletionTokens != 200 || got.TotalToolCalls != 1 {
		t.Fatalf("counters: %+v", got)
	}
}

func TestForeignKeyCascade(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-x", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	_, _ = s.AppendMessage(ctx, Message{InvestigationID: "inv-x", Role: "user", Content: "g"})
	_ = s.InsertToolCall(ctx, ToolCallRow{ID: "tc-x", InvestigationID: "inv-x", Seq: 1, Tool: "list_hosts", InputJSON: `{}`, Status: "pending"})
	_ = s.AddFinding(ctx, Finding{ID: "f-x", InvestigationID: "inv-x", Severity: "info", Code: "x", Message: "y"})

	if _, err := s.db.ExecContext(ctx, `DELETE FROM investigations WHERE id='inv-x'`); err != nil {
		t.Fatal(err)
	}

	msgs, _ := s.ListMessages(ctx, "inv-x", true)
	tcs, _ := s.ListToolCalls(ctx, "inv-x")
	fs, _ := s.ListFindings(ctx, "inv-x")
	if len(msgs) != 0 || len(tcs) != 0 || len(fs) != 0 {
		t.Fatalf("cascade delete failed: msgs=%d tcs=%d fs=%d", len(msgs), len(tcs), len(fs))
	}
}
