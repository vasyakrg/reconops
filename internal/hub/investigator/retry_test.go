package investigator

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// newAbortedInvestigation seeds an aborted investigation with a system + user
// message (the state after a transient callLLM failure: no pending tool_call).
func newAbortedInvestigation(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: id, Goal: "diagnose", Status: "aborted", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: id, Role: "system", Content: "system prompt"})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: id, Role: "user", Content: "etcd flapping"})
}

func askOperatorLLM(t *testing.T) (*llm.Router, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"need scope",
		  "tool_calls":[{"id":"c1","type":"function","function":{"name":"ask_operator","arguments":"{\"question\":\"which host?\"}"}}]},
		  "finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55}}`))
	}))
	t.Setenv("RECON_TEST_RETRY_KEY", "k")
	router, err := llm.NewRouter([]llm.Profile{{
		Name: "primary", Role: "primary", Model: "m", BaseURL: srv.URL,
		APIKeyEnv: "RECON_TEST_RETRY_KEY", SupportsTools: true, MaxOutputTokens: 4096, ContextWindowTokens: 200000,
	}})
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return router, srv.Close
}

func newRetryLoop(st *store.Store, router *llm.Router) *Loop {
	l := &Loop{
		store: st, llm: router.Primary(), maxSteps: 10, maxTokens: 1_000_000,
		running: map[string]bool{}, compactCooldown: map[string]time.Time{},
		log: slog.New(slog.DiscardHandler),
	}
	l.SetRouter(router)
	return l
}

// waitPending blocks until the loop has produced a pending tool_call (the fake
// LLM returns ask_operator, which parks awaiting approval). Once it exists the
// loop has returned and is no longer writing, so a subsequent read is race-free.
func waitPending(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 150; i++ {
		if tc, err := st.PendingToolCall(ctx, id); err == nil && tc != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pending tool_call appeared — loop did not re-run the step")
}

func TestRetryLastStepInjectsNoOperatorMessage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	newAbortedInvestigation(t, st, "inv-retry")

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)

	if err := l.RetryLastStep(ctx, "inv-retry", "operator"); err != nil {
		t.Fatal(err)
	}
	// spawn() re-enters the loop; it returns ask_operator → parks as pending.
	waitPending(t, st, "inv-retry")

	msgs, err := st.ListMessages(ctx, "inv-retry", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "OPERATOR RESUME") {
			t.Fatalf("RetryLastStep must NOT inject an operator turn (that is ResumeAborted's job): %q", m.Content)
		}
	}
}

func TestResumeAbortedInjectsOperatorMessage(t *testing.T) {
	// Contrast: the free-text continue path DOES inject an operator turn.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	newAbortedInvestigation(t, st, "inv-resume")

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)

	if err := l.ResumeAborted(ctx, "inv-resume", "look at disk pressure", "operator"); err != nil {
		t.Fatal(err)
	}
	waitPending(t, st, "inv-resume")

	msgs, err := st.ListMessages(ctx, "inv-resume", true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "OPERATOR RESUME") && strings.Contains(m.Content, "look at disk pressure") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ResumeAborted must inject the operator message as a new turn")
	}
}

func TestRetryLastStepRequiresAborted(t *testing.T) {
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

	// Not aborted → ClaimAbortedForResume loses → no-op, no error, stays active.
	if err := l.RetryLastStep(ctx, "inv-active", "operator"); err != nil {
		t.Fatal(err)
	}
	inv, _ := st.GetInvestigation(ctx, "inv-active")
	if inv.Status != "active" {
		t.Fatalf("retry on a non-aborted investigation must be a no-op, got status %q", inv.Status)
	}
}

func TestAdvanceFlagsTransientLLMError(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-trans", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-trans", Role: "system", Content: "system"})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-trans", Role: "user", Content: "goal"})

	// A server that is created then immediately closed → connection refused →
	// ProviderErrorNetwork (Temporary=true) → abort flagged transient.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := srv.URL
	srv.Close()
	t.Setenv("RECON_TEST_DEAD_KEY", "k")
	router, err := llm.NewRouter([]llm.Profile{{
		Name: "primary", Role: "primary", Model: "m", BaseURL: deadURL,
		APIKeyEnv: "RECON_TEST_DEAD_KEY", SupportsTools: true, MaxOutputTokens: 4096, ContextWindowTokens: 200000,
	}})
	if err != nil {
		t.Fatal(err)
	}
	l := newRetryLoop(st, router)
	l.advance(ctx, "inv-trans") // synchronous; retries with backoff then aborts

	inv, err := st.GetInvestigation(ctx, "inv-trans")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status != "aborted" {
		t.Fatalf("network failure should abort, got %q", inv.Status)
	}
	payload, ok := store.ParseInvestigationTerminalPayload(inv.SummaryJSON)
	if !ok {
		t.Fatal("expected terminal payload")
	}
	if payload.Kind != store.TerminalKindLLMError || payload.Source != "llm" {
		t.Fatalf("expected llm_error/llm, got %+v", payload)
	}
	if !payload.Transient {
		t.Fatalf("a connection-refused abort must be flagged transient for one-click retry: %+v", payload)
	}
}
