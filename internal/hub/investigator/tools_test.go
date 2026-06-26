package investigator

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/store"
)

func TestSystemPromptSubstitutes(t *testing.T) {
	out := BuildSystemPrompt("diagnose etcd", "anthropic/claude-sonnet-4.5",
		time.Date(2026, 4, 18, 10, 0, 0, 0, time.UTC), 40, 500_000)
	for _, want := range []string{
		"diagnose etcd",
		"anthropic/claude-sonnet-4.5",
		"2026-04-18T10:00:00Z",
		"40 tool calls",
		"500000 total tokens",
		"Read-only.",
	} {
		if !contains(out, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if contains(out, "{{") {
		t.Error("placeholder left unsubstituted")
	}
}

func TestToolsHaveValidJSONSchema(t *testing.T) {
	tools := Tools()
	if len(tools) != 12 {
		t.Fatalf("want 12 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		if tl.Type != "function" {
			t.Errorf("%s: type=%s", tl.Function.Name, tl.Type)
		}
		if tl.Function.Name == "" || tl.Function.Description == "" {
			t.Errorf("missing name/desc: %+v", tl)
		}
		if names[tl.Function.Name] {
			t.Errorf("duplicate tool name: %s", tl.Function.Name)
		}
		names[tl.Function.Name] = true
		var schema map[string]any
		if err := json.Unmarshal(tl.Function.Parameters, &schema); err != nil {
			t.Errorf("%s: invalid schema JSON: %v", tl.Function.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s: top-level schema must be type=object", tl.Function.Name)
		}
	}
	mustHave := []string{
		"list_hosts", "list_collectors", "describe_collector",
		"collect", "collect_batch", "search_artifact", "compare_across_hosts", "get_full_result",
		"recall_prior", "add_finding", "ask_operator", "mark_done",
	}
	for _, n := range mustHave {
		if !names[n] {
			t.Errorf("tool %s missing from catalogue", n)
		}
	}
}

func TestMessagesForLLMDropsOrphanToolResults(t *testing.T) {
	tcsJSON, err := json.Marshal([]llm.ToolCall{{
		ID:       "call_visible",
		Type:     "function",
		Function: llm.ToolCallInvocation{Name: "collect", Arguments: `{"host_id":"app01"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}

	out, dropped, _ := messagesForLLM([]store.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "goal"},
		{Role: "tool", Content: `{"ok":true}`, ToolCallID: sql.NullString{String: "call_archived", Valid: true}},
		{Role: "assistant", Content: "next", ToolCallsJSON: sql.NullString{String: string(tcsJSON), Valid: true}},
		{Role: "tool", Content: `{"ok":true}`, ToolCallID: sql.NullString{String: "call_visible", Valid: true}},
	})

	if dropped != 1 {
		t.Fatalf("want 1 dropped orphan tool result, got %d", dropped)
	}
	if len(out) != 4 {
		t.Fatalf("want 4 wire messages, got %d: %+v", len(out), out)
	}
	if out[2].Role != "assistant" || len(out[2].ToolCalls) != 1 || out[2].ToolCalls[0].ID != "call_visible" {
		t.Fatalf("assistant tool call not preserved: %+v", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "call_visible" {
		t.Fatalf("valid tool result not preserved: %+v", out[3])
	}
}

func TestMessagesForLLMDropsToolResultsWithoutCallID(t *testing.T) {
	out, dropped, _ := messagesForLLM([]store.Message{
		{Role: "system", Content: "system"},
		{Role: "tool", Content: `{"ok":true}`},
	})

	if dropped != 1 {
		t.Fatalf("want 1 dropped tool result without call id, got %d", dropped)
	}
	if len(out) != 1 || out[0].Role != "system" {
		t.Fatalf("unexpected wire messages: %+v", out)
	}
}

func TestCloseDanglingToolCallForResumeAppendsToolResult(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-resume", Goal: "g", Status: "aborted", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	callsJSON, err := json.Marshal([]llm.ToolCall{{
		ID:       "call_resume",
		Type:     "function",
		Function: llm.ToolCallInvocation{Name: "collect", Arguments: `{"host_id":"app01"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-resume", Role: "system", Content: "system"})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-resume", Role: "user", Content: "goal"})
	_, _ = st.AppendMessage(ctx, store.Message{
		InvestigationID: "inv-resume", Role: "assistant", Content: "collect logs",
		ToolCallsJSON: sql.NullString{String: string(callsJSON), Valid: true},
	})
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "call_resume", InvestigationID: "inv-resume", Seq: 1,
		Tool: "collect", InputJSON: `{"host_id":"app01"}`, Status: "aborted",
	}); err != nil {
		t.Fatal(err)
	}

	l := &Loop{store: st}
	if err := l.closeDanglingToolCallForResume(ctx, "inv-resume", "operator"); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.ListMessages(ctx, "inv-resume", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || msgs[3].Role != "tool" || !msgs[3].ToolCallID.Valid || msgs[3].ToolCallID.String != "call_resume" {
		t.Fatalf("dangling tool call was not closed: %+v", msgs)
	}
	tc, err := st.GetToolCall(ctx, "call_resume")
	if err != nil {
		t.Fatal(err)
	}
	if tc.Status != "aborted" || !tc.ResultJSON.Valid {
		t.Fatalf("tool call result not recorded: %+v", tc)
	}
	out, dropped, _ := messagesForLLM(msgs)
	if dropped != 0 || len(out) != 4 || out[3].Role != "tool" || out[3].ToolCallID != "call_resume" {
		t.Fatalf("closed history should be wire-safe, dropped=%d out=%+v", dropped, out)
	}
}

func TestMessagesForLLMSynthesizesOutputForDanglingToolCall(t *testing.T) {
	tcsJSON, err := json.Marshal([]llm.ToolCall{{
		ID:       "call_dangling",
		Type:     "function",
		Function: llm.ToolCallInvocation{Name: "collect", Arguments: `{"host_id":"app01"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// An assistant proposed call_dangling, then an operator hypothesis
	// superseded it with no tool result recorded. The provider rejects an
	// assistant function_call that has no following output with
	// "No tool output found for function call call_X" — so the balancer must
	// synthesize a placeholder output, inserted right after the call.
	out, dropped, synthesized := messagesForLLM([]store.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "goal"},
		{Role: "assistant", Content: "propose", ToolCallsJSON: sql.NullString{String: string(tcsJSON), Valid: true}},
		{Role: "user", Content: "OPERATOR HYPOTHESIS [priority: HIGH]\nClaim: check certs"},
	})
	if dropped != 0 {
		t.Fatalf("want 0 dropped, got %d", dropped)
	}
	if synthesized != 1 {
		t.Fatalf("want 1 synthesized output for the dangling call, got %d", synthesized)
	}
	if len(out) != 5 {
		t.Fatalf("want 5 wire messages (synthetic output after the call), got %d: %+v", len(out), out)
	}
	if out[2].Role != "assistant" || len(out[2].ToolCalls) != 1 || out[2].ToolCalls[0].ID != "call_dangling" {
		t.Fatalf("assistant call not preserved: %+v", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "call_dangling" {
		t.Fatalf("synthetic output not inserted immediately after the dangling call: %+v", out[3])
	}
	if out[4].Role != "user" {
		t.Fatalf("operator hypothesis turn should follow the balanced call: %+v", out[4])
	}
}

func TestInjectHypothesisBalancesSupersededPendingToolCall(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-inject", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	callsJSON, err := json.Marshal([]llm.ToolCall{{
		ID:       "call_pending",
		Type:     "function",
		Function: llm.ToolCallInvocation{Name: "collect", Arguments: `{"host_id":"app01"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-inject", Role: "system", Content: "system"})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-inject", Role: "user", Content: "goal"})
	_, _ = st.AppendMessage(ctx, store.Message{
		InvestigationID: "inv-inject", Role: "assistant", Content: "collect logs",
		ToolCallsJSON: sql.NullString{String: string(callsJSON), Valid: true},
	})
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "call_pending", InvestigationID: "inv-inject", Seq: 1,
		Tool: "collect", InputJSON: `{"host_id":"app01"}`, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	// llm non-nil so InjectHypothesis passes its guard; running[inv]=true makes
	// the spawned advance() a no-op, so no async worker mutates history before
	// we assert. nb writes to a throwaway temp dir.
	l := &Loop{
		store:   st,
		llm:     &llm.Client{},
		running: map[string]bool{"inv-inject": true},
		nb:      NewNotebook(t.TempDir(), nil),
	}
	if err := l.InjectHypothesis(ctx, "inv-inject",
		"kube-controller-manager stopped renewing certs", "check apiserver.crt expiry", "verify first", "operator"); err != nil {
		t.Fatal(err)
	}

	tc, err := st.GetToolCall(ctx, "call_pending")
	if err != nil {
		t.Fatal(err)
	}
	if tc.Status != "aborted" {
		t.Fatalf("superseded pending should be aborted, got %q", tc.Status)
	}

	// The history must be wire-safe WITHOUT relying on the synthesis safety net —
	// InjectHypothesis itself appended the function_call_output.
	msgs, err := st.ListMessages(ctx, "inv-inject", true)
	if err != nil {
		t.Fatal(err)
	}
	out, dropped, synthesized := messagesForLLM(msgs)
	if dropped != 0 || synthesized != 0 {
		t.Fatalf("InjectHypothesis must leave a balanced history (dropped=%d synthesized=%d): %+v", dropped, synthesized, out)
	}
	if len(out) != 5 || out[3].Role != "tool" || out[3].ToolCallID != "call_pending" {
		t.Fatalf("function_call_output for the superseded call not appended: %+v", out)
	}
	if out[4].Role != "user" || !strings.Contains(out[4].Content, "OPERATOR HYPOTHESIS") {
		t.Fatalf("operator hypothesis turn missing/misordered: %+v", out[4])
	}
}

func TestAdvanceRecoversPanicAndAbortsInvestigation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-panic", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-panic", Role: "system", Content: "system"})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-panic", Role: "user", Content: "goal"})

	l := &Loop{
		store:           st,
		maxSteps:        10,
		maxTokens:       100000,
		running:         map[string]bool{},
		compactCooldown: map[string]time.Time{},
	}
	l.advance(ctx, "inv-panic")

	inv, err := st.GetInvestigation(ctx, "inv-panic")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status != "aborted" {
		t.Fatalf("panic should abort investigation instead of leaving it active: %+v", inv)
	}
	if !inv.SummaryJSON.Valid || !contains(inv.SummaryJSON.String, "investigator panic") {
		t.Fatalf("panic reason should be persisted for operator recovery: %+v", inv)
	}
}

func TestFinishTerminalPublishesTerminalEvent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-terminal-event", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}

	bus := NewBus()
	ch, unsubscribe := bus.Subscribe("inv-terminal-event")
	defer unsubscribe()
	l := &Loop{store: st, bus: bus}
	if err := l.finishTerminal(ctx, "inv-terminal-event", "aborted",
		store.NewInvestigationTerminalPayload(
			store.TerminalKindLLMError,
			"llm http 502",
			"provider returned 502",
			true,
			"llm",
			time.Now().UTC(),
		)); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-ch:
		if ev.Type != EventTerminal {
			t.Fatalf("first event should be terminal, got %s", ev.Type)
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["kind"] != store.TerminalKindLLMError || payload["reason"] != "llm http 502" ||
			payload["source"] != "llm" || payload["recoverable"] != true {
			t.Fatalf("terminal payload missing fields: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for terminal event")
	}
}

func TestExecuteApprovedAddFindingPublishesFindingAdded(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-finding-event", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRun(ctx, store.Run{
		ID: "run-finding-event", InvestigationID: sql.NullString{String: "inv-finding-event", Valid: true},
		Name: "collect", CreatedBy: "o", Status: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHost(ctx, store.Host{
		ID: "h1", Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "fp", Status: "online",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, store.Task{
		ID: "task-finding-event", RunID: "run-finding-event", HostID: "h1", Collector: "system_info", Status: "done",
	}); err != nil {
		t.Fatal(err)
	}
	args := `{"severity":"warn","code":"router.bad_gateway","message":"router returned 502","evidence_refs":["task-finding-event"]}`
	tc := store.ToolCallRow{
		ID: "tc-finding-event", InvestigationID: "inv-finding-event", Seq: 1,
		Tool: "add_finding", InputJSON: args, Status: "approved",
	}
	if err := st.InsertToolCall(ctx, tc); err != nil {
		t.Fatal(err)
	}

	bus := NewBus()
	ch, unsubscribe := bus.Subscribe("inv-finding-event")
	defer unsubscribe()
	l := &Loop{
		store: st,
		bus:   bus,
	}
	if err := l.executeApproved(ctx, "inv-finding-event", &tc); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-ch:
		if ev.Type != EventFindingAdded {
			t.Fatalf("first event should be finding.added, got %s", ev.Type)
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["code"] != "router.bad_gateway" || payload["severity"] != "warn" || payload["message"] != "router returned 502" {
			t.Fatalf("finding.added payload missing fields: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for finding.added")
	}
}

func TestCompactWritesDurableMemory(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-compact-memory", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-compact-memory", Role: "system", Content: "system"})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-compact-memory", Role: "user", Content: "goal"})
	for i := 0; i < compactionKeepRecent+4; i++ {
		_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-compact-memory", Role: "assistant", Content: "evidence"})
	}
	fakeLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"compact-test",
			"model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"summary of older evidence"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}
		}`))
	}))
	t.Cleanup(fakeLLM.Close)
	client, err := llm.New(llm.Options{BaseURL: fakeLLM.URL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	l := NewLoop(st, client, nil, nil, nil, 10, 100000, nil)
	if err := l.compact(ctx, "inv-compact-memory"); err != nil {
		t.Fatal(err)
	}
	memories, err := st.ListMemory(ctx, "inv-compact-memory", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].Kind != store.MemoryKindContextSummary ||
		!contains(memories[0].Content, "summary of older evidence") {
		t.Fatalf("memory not written: %+v", memories)
	}
	msgs, err := st.ListMessages(ctx, "inv-compact-memory", false)
	if err != nil {
		t.Fatal(err)
	}
	foundSummary := false
	for _, msg := range msgs {
		if msg.Role == "system_summary" && contains(msg.Content, memories[0].ID) {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Fatalf("live system_summary should reference memory id %s: %+v", memories[0].ID, msgs)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
