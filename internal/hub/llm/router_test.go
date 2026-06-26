package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okChatHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`))
	}
}

func TestRouterSelect(t *testing.T) {
	t.Setenv("RK", "key")
	profiles := []Profile{
		{Name: "primary", Role: "primary", Model: "p", BaseURL: "https://p.example", APIKeyEnv: "RK", SupportsTools: true},
		{Name: "sum", Role: "summarizer", Model: "s", BaseURL: "https://s.example", APIKeyEnv: "RK"},
		{Name: "cheapo", Role: "cheap", Model: "c", BaseURL: "https://c.example", APIKeyEnv: "RK"},
	}
	r, err := NewRouter(profiles)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		op       string
		tools    bool
		forced   string
		wantName string
	}{
		{OpPlanNextStep, true, "", "primary"},
		{OpCompactMemory, false, "", "sum"},
		{OpLogTriageSummary, false, "", "cheapo"},
		{OpVerifyFinding, false, "", "primary"},     // no verifier configured → primary
		{OpCompactMemory, true, "", "primary"},      // requireTools → tool-capable primary
		{OpPlanNextStep, true, "cheapo", "primary"}, // forced non-tool profile + requireTools → primary
		{OpPlanNextStep, false, "cheapo", "cheapo"}, // forced honored when tools not required
	}
	for _, c := range cases {
		if got := r.Select(c.op, c.tools, c.forced).Profile; got != c.wantName {
			t.Errorf("Select(%s,tools=%v,forced=%q)=%q want %q", c.op, c.tools, c.forced, got, c.wantName)
		}
	}
}

func TestRouterChatFallback(t *testing.T) {
	t.Setenv("RK", "key")
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer bad.Close()
	good := httptest.NewServer(okChatHandler())
	defer good.Close()

	profiles := []Profile{
		{Name: "primary", Role: "primary", Model: "p", BaseURL: good.URL, APIKeyEnv: "RK", SupportsTools: true},
		{Name: "sum", Role: "summarizer", Model: "s", BaseURL: bad.URL, APIKeyEnv: "RK"},
	}
	r, err := NewRouter(profiles)
	if err != nil {
		t.Fatal(err)
	}
	resp, sel, fallback, err := r.Chat(context.Background(), OpCompactMemory, false, "", ChatRequest{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if sel.Profile != "primary" || fallback != 1 {
		t.Fatalf("expected fallback to primary, got sel=%s fallback=%d", sel.Profile, fallback)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("unexpected response: %+v", resp.Choices)
	}
}

func TestUsageCachedTokens(t *testing.T) {
	var u Usage
	if u.CachedTokens() != 0 {
		t.Fatal("nil details should report 0 cached tokens")
	}
	u.PromptTokensDetails = &PromptTokensDetails{CachedTokens: 9}
	if u.CachedTokens() != 9 {
		t.Fatalf("got %d want 9", u.CachedTokens())
	}
}
