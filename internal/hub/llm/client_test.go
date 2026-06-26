package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessageMarshalDefaultIsByteIdenticalWhenCacheDisabled(t *testing.T) {
	// Without CacheControl the wire bytes must match a plain string-content
	// message exactly, so OpenAI/vLLM and non-cache routes see no new fields.
	m := Message{Role: "system", Content: "you are recon"}
	got, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"role":"system","content":"you are recon"}`
	if string(got) != want {
		t.Fatalf("disabled path drifted:\n got=%s\nwant=%s", got, want)
	}

	// A tool message round-trips identically too.
	tm := Message{Role: "tool", Content: `{"ok":true}`, ToolCallID: "call_1"}
	gotTool, _ := json.Marshal(tm)
	const wantTool = `{"role":"tool","content":"{\"ok\":true}","tool_call_id":"call_1"}`
	if string(gotTool) != wantTool {
		t.Fatalf("tool disabled path drifted:\n got=%s\nwant=%s", gotTool, wantTool)
	}
}

func TestMessageMarshalEmitsCacheControlBlock(t *testing.T) {
	m := Message{Role: "system", Content: "stable prefix", CacheControl: true}
	got, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Role    string `json:"role"`
		Content []struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			CacheControl struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"content"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("cache-control message did not produce a content block array: %v (%s)", err, got)
	}
	if decoded.Role != "system" || len(decoded.Content) != 1 {
		t.Fatalf("unexpected shape: %s", got)
	}
	b := decoded.Content[0]
	if b.Type != "text" || b.Text != "stable prefix" || b.CacheControl.Type != "ephemeral" {
		t.Fatalf("cache_control block wrong: %s", got)
	}
}

func TestMessageMarshalCacheControlNoopOnEmptyContent(t *testing.T) {
	// An assistant message with only tool_calls and no content must NOT be
	// coerced into a content-block array even if CacheControl is set.
	m := Message{Role: "assistant", CacheControl: true, ToolCalls: []ToolCall{{
		ID: "c1", Type: "function", Function: ToolCallInvocation{Name: "collect", Arguments: "{}"},
	}}}
	got, _ := json.Marshal(m)
	if strings.Contains(string(got), "cache_control") {
		t.Fatalf("empty-content message must not emit cache_control: %s", got)
	}
}

func TestListModelsReadsContextWindowPerProvider(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]int
	}{
		{
			name: "openrouter context_length",
			body: `{"data":[{"id":"anthropic/claude-sonnet-4.5","context_length":200000},{"id":"x","context_length":0}]}`,
			want: map[string]int{"anthropic/claude-sonnet-4.5": 200000},
		},
		{
			name: "vllm max_model_len",
			body: `{"data":[{"id":"gpt-5.5","max_model_len":262144}]}`,
			want: map[string]int{"gpt-5.5": 262144},
		},
		{
			name: "raw openai has no window field -> empty",
			body: `{"data":[{"id":"gpt-4o","object":"model","owned_by":"openai"}]}`,
			want: map[string]int{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models" || r.Method != http.MethodGet {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer k" {
					t.Errorf("missing auth header: %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, err := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.ListModels(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("window[%q]=%d want %d", k, got[k], v)
				}
			}
		})
	}
}

func TestListModelsSoftMissOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("expected an error the caller can treat as a soft miss")
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := New(Options{BaseURL: "https://x", Model: "m"}); err == nil {
		t.Fatal("expected error for missing key")
	}
	if _, err := New(Options{APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("expected error for missing base_url")
	}
	if _, err := New(Options{APIKey: "k", BaseURL: "https://x"}); err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestNewHTTPURLPolicy(t *testing.T) {
	tests := []struct {
		name              string
		baseURL           string
		allowInsecureHTTP bool
		wantErr           bool
	}{
		{
			name:    "public HTTP hostname rejected by default",
			baseURL: "http://openrouter.ai/api/v1",
			wantErr: true,
		},
		{
			name:              "public HTTP hostname rejected with opt-in",
			baseURL:           "http://openrouter.ai/api/v1",
			allowInsecureHTTP: true,
			wantErr:           true,
		},
		{
			name:              "public HTTP IP rejected with opt-in",
			baseURL:           "http://8.8.8.8/v1",
			allowInsecureHTTP: true,
			wantErr:           true,
		},
		{
			name:    "private HTTP IP rejected without opt-in",
			baseURL: "http://192.168.50.10:8080/v1",
			wantErr: true,
		},
		{
			name:              "private HTTP IP allowed with opt-in",
			baseURL:           "http://192.168.50.10:8080/v1",
			allowInsecureHTTP: true,
		},
		{
			name:              "link-local HTTP IP allowed with opt-in",
			baseURL:           "http://169.254.10.20:8080/v1",
			allowInsecureHTTP: true,
		},
		{
			name:              "IPv6 ULA HTTP IP allowed with opt-in",
			baseURL:           "http://[fd00::1]:8080/v1",
			allowInsecureHTTP: true,
		},
		{
			name:    "loopback IPv4 HTTP allowed without opt-in",
			baseURL: "http://127.0.0.1:8080/v1",
		},
		{
			name:    "localhost HTTP allowed without opt-in",
			baseURL: "http://localhost:8080/v1",
		},
		{
			name:    "loopback IPv6 HTTP allowed without opt-in",
			baseURL: "http://[::1]:8080/v1",
		},
		{
			name:    "HTTPS allowed",
			baseURL: "https://openrouter.ai/api/v1",
		},
		{
			name:    "malformed URL rejected",
			baseURL: "http://[::1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Options{
				APIKey:            "k",
				BaseURL:           tt.baseURL,
				Model:             "m",
				AllowInsecureHTTP: tt.allowInsecureHTTP,
			})
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected allow: %v", err)
			}
		})
	}
}

func TestChatRoundtrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth=%q", got)
		}
		if got := r.Header.Get("X-Title"); got != "Recon" {
			t.Errorf("xtitle=%q", got)
		}
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "anthropic/claude-sonnet-4.5" {
			t.Errorf("model=%q", req.Model)
		}
		if len(req.Tools) != 1 || req.Tools[0].Function.Name != "list_hosts" {
			t.Errorf("tools=%+v", req.Tools)
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{
			ID:    "resp-1",
			Model: req.Model,
			Choices: []Choice{{
				Index: 0,
				Message: Message{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID: "call-1", Type: "function",
						Function: ToolCallInvocation{Name: "list_hosts", Arguments: `{}`},
					}},
				},
				FinishReason: "tool_calls",
			}},
			Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		})
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL: srv.URL, APIKey: "k", Model: "anthropic/claude-sonnet-4.5",
		XTitle: "Recon",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name: "list_hosts", Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		}}},
		ToolChoice: "required",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.ToolCalls[0].Function.Name != "list_hosts" {
		t.Fatalf("got %+v", resp)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("usage: %+v", resp.Usage)
	}
}

func TestSanitizeForError(t *testing.T) {
	apiKey := "sk-or-v1-abc123def456ghi789" //nolint:gosec // test fixture, not a real credential
	body := []byte(`{"error":"key sk-or-v1-abc123def456ghi789 invalid; also seen sk-ant-xyz9876543210abcd"}`)
	got := sanitizeForError(body, apiKey)
	if strings.Contains(string(got), apiKey) {
		t.Errorf("apiKey leaked: %s", got)
	}
	if strings.Contains(string(got), "sk-ant-xyz") {
		t.Errorf("provider key shape leaked: %s", got)
	}
	if !strings.Contains(string(got), "REDACTED") {
		t.Errorf("expected redaction marker: %s", got)
	}
}

func TestChatErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "bad", Model: "m"})
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401-bearing error, got %v", err)
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T %[1]v", err)
	}
	if providerErr.Class != ProviderErrorAuth || providerErr.StatusCode() != http.StatusUnauthorized || providerErr.Temporary() {
		t.Fatalf("wrong provider error classification: %+v", providerErr)
	}
	if !strings.Contains(providerErr.SafeDetail(), "invalid api key") {
		t.Fatalf("safe detail missing provider message: %q", providerErr.SafeDetail())
	}
}

func TestChatProviderErrorClassifiesRetryableAndContextLimit(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantClass ProviderErrorClass
		wantTemp  bool
	}{
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"error":"slow down"}`, wantClass: ProviderErrorRateLimit, wantTemp: true},
		{name: "server", status: http.StatusBadGateway, body: `{"error":"bad gateway"}`, wantClass: ProviderErrorServer, wantTemp: true},
		{name: "context", status: http.StatusBadRequest, body: `{"error":"context window limit exceeded"}`, wantClass: ProviderErrorContextLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m"})
			_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("expected ProviderError, got %T %[1]v", err)
			}
			if providerErr.Class != tt.wantClass || providerErr.Temporary() != tt.wantTemp {
				t.Fatalf("class/temp = %s/%v, want %s/%v", providerErr.Class, providerErr.Temporary(), tt.wantClass, tt.wantTemp)
			}
		})
	}
}

func TestChatDecodeErrorIsProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T %[1]v", err)
	}
	// A 2xx body we can't parse is treated as a transient gateway hiccup and
	// is retryable, so a single flaky response doesn't abort the investigation.
	if providerErr.Class != ProviderErrorDecode || !providerErr.Temporary() {
		t.Fatalf("decode should be retryable: %+v", providerErr)
	}
}

func TestChatEmptyChoicesRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"m","choices":[]}`))
	}))
	defer srv.Close()
	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T %[1]v", err)
	}
	if providerErr.Class != ProviderErrorDecode || !providerErr.Temporary() {
		t.Fatalf("empty choices should be retryable: %+v", providerErr)
	}
}

func TestNewHTTPClient_NoProxyKeepsDefault(t *testing.T) {
	c, err := newHTTPClient(defaultTimeout, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Transport != nil {
		t.Fatalf("no proxy must leave Transport nil (default), got %T", c.Transport)
	}
	if c.Timeout != defaultTimeout {
		t.Fatalf("timeout not propagated: %v", c.Timeout)
	}
}

func TestNewHTTPClient_HTTPProxy(t *testing.T) {
	c, err := newHTTPClient(defaultTimeout, "http://proxy.example:3128")
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("want *http.Transport, got %T", c.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("http proxy must set Transport.Proxy")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.cerebras.ai/v1/chat/completions", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil || u.Host != "proxy.example:3128" {
		t.Fatalf("proxy func resolved wrong target: u=%v err=%v", u, err)
	}
}

func TestNewHTTPClient_SOCKS5ResolvesViaProxy(t *testing.T) {
	// Both socks5 and socks5h must wire a DialContext (the SOCKS dialer hands
	// the hostname to the proxy → DNS-over-SOCKS) and must NOT also honor an
	// env HTTP proxy.
	for _, scheme := range []string{"socks5", "socks5h"} {
		c, err := newHTTPClient(defaultTimeout, scheme+"://user:pass@127.0.0.1:1080")
		if err != nil {
			t.Fatalf("%s: %v", scheme, err)
		}
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: want *http.Transport, got %T", scheme, c.Transport)
		}
		if tr.DialContext == nil {
			t.Fatalf("%s: SOCKS5 must set Transport.DialContext", scheme)
		}
		if tr.Proxy != nil {
			t.Fatalf("%s: SOCKS5 must clear Transport.Proxy so env proxy can't double-apply", scheme)
		}
	}
}

func TestNewHTTPClient_RejectsBadProxy(t *testing.T) {
	for _, bad := range []string{
		"ftp://proxy:21",      // unsupported scheme
		"socks4://proxy:1080", // unsupported scheme
		"http://",             // no host
		"://nope",             // unparseable
	} {
		if _, err := newHTTPClient(defaultTimeout, bad); err == nil {
			t.Fatalf("expected error for proxy %q", bad)
		}
	}
}

func TestNewFromEnv_ReadsProxyEnv(t *testing.T) {
	t.Setenv("RECON_LLM_API_KEY", "k")
	t.Setenv("RECON_LLM_PROXY_TYPE", "socks5h")
	t.Setenv("RECON_LLM_PROXY_ADDR", "127.0.0.1:1080")
	c, err := NewFromEnv("https://api.cerebras.ai/v1", "m", "RECON_LLM_API_KEY", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatalf("proxy env not applied via NewFromEnv: transport=%T", c.httpClient.Transport)
	}
}

func TestTimeoutFromEnv(t *testing.T) {
	// Unset → 0 so New falls back to defaultTimeout.
	t.Setenv("RECON_LLM_TIMEOUT", "")
	if d, err := timeoutFromEnv(); err != nil || d != 0 {
		t.Fatalf("empty must yield 0: d=%v err=%v", d, err)
	}
	// Go duration string.
	t.Setenv("RECON_LLM_TIMEOUT", "3m")
	if d, err := timeoutFromEnv(); err != nil || d != 3*time.Minute {
		t.Fatalf("duration parse: d=%v err=%v", d, err)
	}
	// Bare integer = seconds.
	t.Setenv("RECON_LLM_TIMEOUT", "240")
	if d, err := timeoutFromEnv(); err != nil || d != 240*time.Second {
		t.Fatalf("integer seconds: d=%v err=%v", d, err)
	}
	// Non-positive is a hard error, not a silent default.
	t.Setenv("RECON_LLM_TIMEOUT", "0")
	if _, err := timeoutFromEnv(); err == nil {
		t.Fatal("zero must error")
	}
	t.Setenv("RECON_LLM_TIMEOUT", "-5s")
	if _, err := timeoutFromEnv(); err == nil {
		t.Fatal("negative must error")
	}
	// Garbage errors.
	t.Setenv("RECON_LLM_TIMEOUT", "soon")
	if _, err := timeoutFromEnv(); err == nil {
		t.Fatal("garbage must error")
	}
}

func TestNewFromEnv_AppliesTimeout(t *testing.T) {
	t.Setenv("RECON_LLM_API_KEY", "k")
	t.Setenv("RECON_LLM_TIMEOUT", "300s")
	c, err := NewFromEnv("https://openrouter.ai/api/v1", "m", "RECON_LLM_API_KEY", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 300*time.Second {
		t.Fatalf("timeout not applied via NewFromEnv: %v", c.Timeout())
	}
	// Unset → defaultTimeout.
	t.Setenv("RECON_LLM_TIMEOUT", "")
	c, err = NewFromEnv("https://openrouter.ai/api/v1", "m", "RECON_LLM_API_KEY", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != defaultTimeout {
		t.Fatalf("expected defaultTimeout fallback, got %v", c.Timeout())
	}
}

func TestProxyURLFromEnv(t *testing.T) {
	// Unset type → no proxy.
	t.Setenv("RECON_LLM_PROXY_TYPE", "")
	if u, err := proxyURLFromEnv(); err != nil || u != "" {
		t.Fatalf("empty type must yield no proxy: u=%q err=%v", u, err)
	}
	// Separate fields assemble into the internal URL, credentials and all.
	t.Setenv("RECON_LLM_PROXY_TYPE", "socks5h")
	t.Setenv("RECON_LLM_PROXY_ADDR", "proxy.example:1080")
	t.Setenv("RECON_LLM_PROXY_USER", "alice")
	t.Setenv("RECON_LLM_PROXY_PASS", "s3cr3t")
	u, err := proxyURLFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if u != "socks5h://alice:s3cr3t@proxy.example:1080" {
		t.Fatalf("assembled URL wrong: %q", u)
	}
	// Type set but addr missing is a clear error, not a silent direct connection.
	t.Setenv("RECON_LLM_PROXY_ADDR", "")
	if _, err := proxyURLFromEnv(); err == nil {
		t.Fatal("type without addr must error")
	}
	// Unsupported type errors.
	t.Setenv("RECON_LLM_PROXY_TYPE", "socks4")
	t.Setenv("RECON_LLM_PROXY_ADDR", "proxy.example:1080")
	if _, err := proxyURLFromEnv(); err == nil {
		t.Fatal("unsupported type must error")
	}
}
