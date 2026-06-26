package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func TestMessageToolCallsRoundtrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-tc", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	tcsJSON := `[{"id":"call_1","type":"function","function":{"name":"list_hosts","arguments":"{}"}}]`
	if _, err := s.AppendMessage(ctx, Message{
		InvestigationID: "inv-tc", Role: "assistant", Content: "let me check the inventory first",
		ToolCallsJSON: sql.NullString{String: tcsJSON, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(ctx, "inv-tc", false)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list: %v len=%d", err, len(msgs))
	}
	if !msgs[0].ToolCallsJSON.Valid || msgs[0].ToolCallsJSON.String != tcsJSON {
		t.Fatalf("tool_calls_json round-trip failed: %+v", msgs[0])
	}
	if msgs[0].Content != "let me check the inventory first" {
		t.Fatalf("content lost: %q", msgs[0].Content)
	}
}

func TestReactivateInvestigationClearsTerminalSummary(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-resume", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	if err := s.FinishInvestigation(ctx, "inv-resume", "aborted", `{"error":"llm http 502"}`); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetInvestigation(ctx, "inv-resume")
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != "aborted" || !before.SummaryJSON.Valid {
		t.Fatalf("expected aborted with summary, got %+v", before)
	}
	if err := s.ReactivateInvestigation(ctx, "inv-resume"); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetInvestigation(ctx, "inv-resume")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "active" {
		t.Fatalf("expected active, got %+v", after)
	}
	if after.SummaryJSON.Valid {
		t.Fatalf("terminal summary should be cleared, got %q", after.SummaryJSON.String)
	}
}

func TestClaimAbortedForResumeIsIdempotent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-claim", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	// Not aborted → cannot be claimed.
	if ok, err := s.ClaimAbortedForResume(ctx, "inv-claim"); err != nil || ok {
		t.Fatalf("active investigation: claim=%v err=%v, want false/nil", ok, err)
	}
	if err := s.FinishInvestigation(ctx, "inv-claim", "aborted", `{"kind":"llm_error"}`); err != nil {
		t.Fatal(err)
	}
	// First claim wins; a concurrent double-submit loses (idempotent gate).
	ok1, err := s.ClaimAbortedForResume(ctx, "inv-claim")
	if err != nil || !ok1 {
		t.Fatalf("first claim=%v err=%v, want true/nil", ok1, err)
	}
	ok2, err := s.ClaimAbortedForResume(ctx, "inv-claim")
	if err != nil || ok2 {
		t.Fatalf("second claim=%v err=%v, want false/nil (double-submit must be a no-op)", ok2, err)
	}
	after, err := s.GetInvestigation(ctx, "inv-claim")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "active" || after.SummaryJSON.Valid {
		t.Fatalf("after claim want active + cleared summary, got status=%s summaryValid=%v", after.Status, after.SummaryJSON.Valid)
	}
}

// A DONE investigation is reopenable in place: ClaimReopenableForResume flips it
// to active and clears the stale mark_done summary, and is idempotent against a
// double-submit.
func TestClaimReopenableForResume_Done(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-reopen-done", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	if err := s.FinishInvestigation(ctx, "inv-reopen-done", "done", `{"kind":"done","summary":{"root_cause":"tpm storm"}}`); err != nil {
		t.Fatal(err)
	}
	ok1, err := s.ClaimReopenableForResume(ctx, "inv-reopen-done")
	if err != nil || !ok1 {
		t.Fatalf("first reopen of done claim=%v err=%v, want true/nil", ok1, err)
	}
	ok2, err := s.ClaimReopenableForResume(ctx, "inv-reopen-done")
	if err != nil || ok2 {
		t.Fatalf("second reopen=%v err=%v, want false/nil (double-submit must no-op)", ok2, err)
	}
	after, err := s.GetInvestigation(ctx, "inv-reopen-done")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "active" || after.SummaryJSON.Valid {
		t.Fatalf("after reopen want active + cleared summary, got status=%s summaryValid=%v", after.Status, after.SummaryJSON.Valid)
	}
}

// The generalized claim still reopens aborted (no regression) and refuses
// non-terminal states.
func TestClaimReopenableForResume_AbortedAndNonTerminal(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-reopen-mix", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	// active → not reopenable.
	if ok, err := s.ClaimReopenableForResume(ctx, "inv-reopen-mix"); err != nil || ok {
		t.Fatalf("active claim=%v err=%v, want false/nil", ok, err)
	}
	if err := s.FinishInvestigation(ctx, "inv-reopen-mix", "aborted", `{"kind":"llm_error"}`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimReopenableForResume(ctx, "inv-reopen-mix"); err != nil || !ok {
		t.Fatalf("aborted reopen=%v err=%v, want true/nil (no regression)", ok, err)
	}
}

func TestListRecentDoneInvestigations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mk := func(id, status string, hosts []string, summary string) {
		if err := s.InsertInvestigation(ctx, Investigation{
			ID: id, Goal: "g-" + id, Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u", AllowedHosts: hosts,
		}); err != nil {
			t.Fatal(err)
		}
		if status != "active" {
			if err := s.FinishInvestigation(ctx, id, status, summary); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("d1", "done", []string{"h1"}, TerminalDonePayload(`{"root_cause":"rc1"}`, time.Time{}).JSON())
	mk("d2", "done", []string{"h2"}, "") // legacy/empty summary — must be tolerated, not panic
	mk("a1", "aborted", nil, `{"error":"x"}`)
	mk("act", "active", nil, "")

	// Excludes d2 → only the other done investigation (d1) remains.
	got, err := s.ListRecentDoneInvestigations(ctx, "d2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "d1" {
		t.Fatalf("want [d1] (done, excluding d2), got %+v", got)
	}
	if len(got[0].AllowedHosts) != 1 || got[0].AllowedHosts[0] != "h1" {
		t.Fatalf("allowed_hosts not parsed: %+v", got[0].AllowedHosts)
	}
	if p, ok := ParseInvestigationTerminalPayload(got[0].SummaryJSON); !ok || !strings.Contains(p.Reason, "rc1") {
		t.Fatalf("summary should parse and carry the root cause, got ok=%v reason=%q", ok, p.Reason)
	}

	// Both done investigations returned (legacy NULL/empty summary tolerated);
	// aborted + active are excluded.
	all, err := s.ListRecentDoneInvestigations(ctx, "none", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 done investigations, got %d: %+v", len(all), all)
	}
}

func TestSetAndGetInvestigationPriors(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.InsertInvestigation(ctx, Investigation{ID: "inv-p", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInvestigationPriors(ctx, "inv-p", []string{"prior-a", "prior-b"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInvestigation(ctx, "inv-p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Priors) != 2 || got.Priors[0] != "prior-a" || got.Priors[1] != "prior-b" {
		t.Fatalf("priors round-trip failed, got %+v", got.Priors)
	}
	if err := s.SetInvestigationPriors(ctx, "inv-p", nil); err != nil {
		t.Fatal(err)
	}
	if got2, _ := s.GetInvestigation(ctx, "inv-p"); len(got2.Priors) != 0 {
		t.Fatalf("priors should be cleared, got %+v", got2.Priors)
	}
}

func TestListInvestigationsByIDs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mk := func(id, status string, hosts []string) {
		if err := s.InsertInvestigation(ctx, Investigation{ID: id, Goal: "g-" + id, Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u", AllowedHosts: hosts}); err != nil {
			t.Fatal(err)
		}
		if status != "active" {
			if err := s.FinishInvestigation(ctx, id, status, TerminalDonePayload(`{"root_cause":"rc-`+id+`"}`, time.Time{}).JSON()); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("d1", "done", []string{"h1"})
	mk("a1", "aborted", nil)

	// ANY status is returned now (aborted included for manual priors); "missing"
	// is silently skipped.
	got, err := s.ListInvestigationsByIDs(ctx, []string{"d1", "a1", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want d1 + a1 (missing skipped), got %d: %+v", len(got), got)
	}
	byID := map[string]PriorInvestigation{}
	for _, p := range got {
		byID[p.ID] = p
	}
	if byID["d1"].Status != "done" || byID["a1"].Status != "aborted" {
		t.Fatalf("status must be populated: d1=%q a1=%q", byID["d1"].Status, byID["a1"].Status)
	}
	if len(byID["d1"].AllowedHosts) != 1 || byID["d1"].AllowedHosts[0] != "h1" {
		t.Fatalf("hosts not parsed: %+v", byID["d1"].AllowedHosts)
	}
}

func TestListRecentInvestigationsForPriors(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mk := func(id, status string) {
		if err := s.InsertInvestigation(ctx, Investigation{ID: id, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
			t.Fatal(err)
		}
		if status != "active" {
			if err := s.FinishInvestigation(ctx, id, status, `{"kind":"`+status+`"}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("d", "done")
	mk("a", "aborted")
	mk("act", "active")

	got, err := s.ListRecentInvestigationsForPriors(ctx, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want all 3 statuses as manual-picker candidates, got %d: %+v", len(got), got)
	}
	statuses := map[string]bool{}
	for _, p := range got {
		statuses[p.Status] = true
	}
	for _, want := range []string{"done", "aborted", "active"} {
		if !statuses[want] {
			t.Fatalf("missing status %q among candidates: %+v", want, got)
		}
	}
	if g2, _ := s.ListRecentInvestigationsForPriors(ctx, "a", 20); len(g2) != 2 {
		t.Fatalf("excludeID must drop that row, got %d", len(g2))
	}
}

func TestInvestigationTerminalPayloadSanitizesAndCaps(t *testing.T) {
	payload := NewInvestigationTerminalPayload(
		TerminalKindLLMError,
		"provider rejected Authorization: Bearer sk_live_1234567890abcdef\ntry again",
		"Cookie: sid=abcdef; api_key=or_live_1234567890abcdef password=hunter2",
		true,
		"llm",
		time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
	)
	if payload.Reason != "provider rejected Authorization=[REDACTED] try again" {
		t.Fatalf("reason not sanitized/one-lined: %q", payload.Reason)
	}
	if containsAny(payload.Detail, []string{"sk_live", "or_live", "hunter2", "abcdef"}) {
		t.Fatalf("detail leaked secret material: %q", payload.Detail)
	}
	if payload.Detail == "" || payload.Kind != TerminalKindLLMError || !payload.Recoverable || payload.Source != "llm" {
		t.Fatalf("payload fields not preserved: %+v", payload)
	}
}

func TestParseInvestigationTerminalPayloadTypedAndLegacy(t *testing.T) {
	typedJSON := NewInvestigationTerminalPayload(
		TerminalKindPanic,
		"panic while handling Authorization: Bearer sk_live_1234567890abcdef",
		"stack omitted",
		true,
		"loop",
		time.Now().UTC(),
	).JSON()
	typed, ok := ParseInvestigationTerminalPayload(sql.NullString{String: typedJSON, Valid: true})
	if !ok {
		t.Fatal("typed payload should parse")
	}
	if typed.Kind != TerminalKindPanic || typed.Source != "loop" || !typed.Recoverable {
		t.Fatalf("typed payload fields lost: %+v", typed)
	}
	if containsAny(typed.Reason, []string{"sk_live", "Bearer "}) {
		t.Fatalf("typed payload leaked secret in reason: %q", typed.Reason)
	}

	legacy, ok := ParseInvestigationTerminalPayload(sql.NullString{String: `{"error":"llm http 502 api_key=or_live_1234567890abcdef"}`, Valid: true})
	if !ok {
		t.Fatal("legacy payload should parse")
	}
	if legacy.Kind != TerminalKindError || legacy.Source != "legacy" || !legacy.Recoverable {
		t.Fatalf("legacy error fields wrong: %+v", legacy)
	}
	if containsAny(legacy.Detail, []string{"or_live", "1234567890abcdef"}) {
		t.Fatalf("legacy detail leaked secret: %q", legacy.Detail)
	}

	invalid, ok := ParseInvestigationTerminalPayload(sql.NullString{String: `{not-json`, Valid: true})
	if !ok || invalid.Kind != TerminalKindError || invalid.Reason != "invalid terminal payload" {
		t.Fatalf("invalid payload should produce recoverable parse marker: ok=%v payload=%+v", ok, invalid)
	}
}

func TestTerminalDonePayloadPreservesSummaryShape(t *testing.T) {
	summary := `{"root_cause":"expired cert","actions":["renew"]}`
	payload := TerminalDonePayload(summary, time.Now().UTC())
	if payload.Kind != TerminalKindDone || payload.Reason != "Investigation complete: expired cert" {
		t.Fatalf("done reason wrong: %+v", payload)
	}
	var got map[string]any
	if err := json.Unmarshal(payload.Summary, &got); err != nil {
		t.Fatalf("summary should remain valid JSON: %v", err)
	}
	if got["root_cause"] != "expired cert" {
		t.Fatalf("summary root cause changed: %+v", got)
	}
}

func TestInvestigationContextTurnsLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-context", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	id, err := s.InsertContextTurn(ctx, InvestigationContextTurn{
		InvestigationID:       "inv-context",
		MessageSeq:            7,
		Operation:             "plan_next_step",
		ModelProfile:          "primary",
		EstimatedPromptTokens: 1234,
		ContextWindowTokens:   200000,
		ThresholdTokens:       100000,
		ReservedOutputTokens:  4096,
		SafetyHeadroomTokens:  2048,
		ShouldCompact:         true,
		CompactionReason:      "threshold",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateContextTurnUsage(ctx, id, 1200, 300); err != nil {
		t.Fatal(err)
	}
	turns, err := s.ListContextTurns(ctx, "inv-context", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns=%+v", turns)
	}
	got := turns[0]
	if got.ID != id || got.Operation != "plan_next_step" || got.ProviderPromptTokens != 1200 ||
		got.ProviderCompletionTokens != 300 || !got.ShouldCompact || got.CompactionReason != "threshold" {
		t.Fatalf("context turn round-trip failed: %+v", got)
	}
}

func TestInvestigationMemoryLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-memory", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	if err := s.AddMemory(ctx, InvestigationMemory{
		ID: "mem-1", InvestigationID: "inv-memory", Kind: MemoryKindContextSummary,
		Content: "COMPACT_STATE", EvidenceRefsJSON: `{"not":"array"}`,
	}); err == nil {
		t.Fatal("expected non-array evidence refs to be rejected")
	}
	if err := s.AddMemory(ctx, InvestigationMemory{
		ID: "mem-1", InvestigationID: "inv-memory", Kind: MemoryKindContextSummary,
		Content: "COMPACT_STATE", EvidenceRefsJSON: `[]`,
		MessageSeqStart: 3, MessageSeqEnd: 9, TokenEstimate: 42,
	}); err != nil {
		t.Fatal(err)
	}
	memories, err := s.ListMemory(ctx, "inv-memory", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 {
		t.Fatalf("memories=%+v", memories)
	}
	got := memories[0]
	if got.ID != "mem-1" || got.Kind != MemoryKindContextSummary || got.MessageSeqStart != 3 ||
		got.MessageSeqEnd != 9 || got.TokenEstimate != 42 {
		t.Fatalf("memory round-trip failed: %+v", got)
	}
}

func TestToolCallBroadConfirmedFlag(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.InsertInvestigation(ctx, Investigation{
		ID: "inv-bc", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	})
	_ = s.InsertToolCall(ctx, ToolCallRow{
		ID: "tc-bc", InvestigationID: "inv-bc", Seq: 1,
		Tool: "collect_batch", InputJSON: `{"host_ids":["h1","h2"]}`, Status: "pending",
	})
	tc, _ := s.GetToolCall(ctx, "tc-bc")
	if tc.BroadConfirmed {
		t.Fatal("default should be false")
	}
	if err := s.SetToolCallBroadConfirmed(ctx, "tc-bc", true); err != nil {
		t.Fatal(err)
	}
	tc, _ = s.GetToolCall(ctx, "tc-bc")
	if !tc.BroadConfirmed {
		t.Fatal("flag round-trip failed")
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

func containsAny(s string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// SetAutonomousRun records absolute targets and resumes a paused investigation;
// DisarmAutonomous clears them. Both round-trip through GetInvestigation.
func TestAutonomousRunArmDisarm(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.InsertInvestigation(ctx, Investigation{
		ID: "inv-ar", Goal: "g", Status: "paused", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	// Arm: targets set AND a paused investigation flips back to active (re-arm).
	if err := s.SetAutonomousRun(ctx, "inv-ar", 18, 300_000); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInvestigation(ctx, "inv-ar")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRunUntilSteps != 18 || got.AutoRunUntilTokens != 300_000 {
		t.Fatalf("armed targets not persisted: steps=%d tokens=%d", got.AutoRunUntilSteps, got.AutoRunUntilTokens)
	}
	if got.Status != "active" {
		t.Fatalf("re-arm must flip paused → active, got %q", got.Status)
	}
	// Disarm clears the targets; status is left untouched.
	if err := s.DisarmAutonomous(ctx, "inv-ar"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetInvestigation(ctx, "inv-ar")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRunUntilSteps != 0 || got.AutoRunUntilTokens != 0 {
		t.Fatalf("disarm must clear targets: steps=%d tokens=%d", got.AutoRunUntilSteps, got.AutoRunUntilTokens)
	}
	if got.Status != "active" {
		t.Fatalf("disarm must not change status, got %q", got.Status)
	}
}
