package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRequiresKey(t *testing.T) {
	if _, err := New(Options{BaseURL: "http://x", Model: "m"}); err == nil {
		t.Fatal("expected error for missing key")
	}
	if _, err := New(Options{APIKey: "k", Model: "m"}); err == nil {
		t.Fatal("expected error for missing base_url")
	}
	if _, err := New(Options{APIKey: "k", BaseURL: "http://x"}); err == nil {
		t.Fatal("expected error for missing model")
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
}
