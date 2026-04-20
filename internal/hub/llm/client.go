// Package llm is a thin OpenAI-compatible chat-completions client used by
// the investigator. It targets OpenRouter by default but works with any
// compatible endpoint (vLLM, LiteLLM, raw OpenAI, etc.) — the URL, model
// name, and API key are externally configured (env > hub.yaml > compile-in
// default). This deliberately does not pull in the official OpenAI Go SDK
// to keep the dep set minimal and avoid SDK-specific tool-format quirks.
//
// The investigator reasons in OpenAI tool-calling shape ("type":"function",
// JSON Schema input). The system prompt + 11 tool schemas live in the
// investigator package; this file only knows about wire format.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const defaultTimeout = 120 * time.Second

type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
	headers    map[string]string
}

type Options struct {
	BaseURL     string
	APIKey      string
	Model       string
	HTTPReferer string // OpenRouter ranking; harmless on other backends
	XTitle      string // OpenRouter ranking; harmless on other backends
	Timeout     time.Duration
}

// New constructs a Client. Fails if APIKey is empty — without a key any
// request will 401 and we want a clear startup error instead of late failures.
func New(opt Options) (*Client, error) {
	if strings.TrimSpace(opt.APIKey) == "" {
		return nil, errors.New("llm: API key is empty (set RECON_LLM_API_KEY or hub.yaml.llm.api_key_env)")
	}
	if opt.BaseURL == "" {
		return nil, errors.New("llm: base_url is empty")
	}
	if opt.Model == "" {
		return nil, errors.New("llm: model is empty")
	}
	// (H2) Refuse to ship a bearer token over plaintext HTTP unless the
	// endpoint is explicitly loopback (operator running a local LLM gateway
	// is the only legitimate case).
	if !isHTTPSAllowed(opt.BaseURL) {
		return nil, fmt.Errorf("llm: base_url %q is plaintext HTTP and not loopback — bearer token would leak", opt.BaseURL)
	}
	to := opt.Timeout
	if to <= 0 {
		to = defaultTimeout
	}
	headers := map[string]string{}
	if opt.HTTPReferer != "" {
		headers["HTTP-Referer"] = opt.HTTPReferer
	}
	if opt.XTitle != "" {
		headers["X-Title"] = opt.XTitle
	}
	return &Client{
		baseURL:    strings.TrimRight(opt.BaseURL, "/"),
		apiKey:     opt.APIKey,
		model:      opt.Model,
		httpClient: &http.Client{Timeout: to},
		headers:    headers,
	}, nil
}

// NewFromEnv resolves API key from envName at the moment of construction.
func NewFromEnv(baseURL, model, envName, referer, title string) (*Client, error) {
	if envName == "" {
		envName = "RECON_LLM_API_KEY"
	}
	return New(Options{
		BaseURL:     baseURL,
		APIKey:      os.Getenv(envName),
		Model:       model,
		HTTPReferer: referer,
		XTitle:      title,
	})
}

func (c *Client) Model() string   { return c.model }
func (c *Client) BaseURL() string { return c.baseURL }

// ChatRequest is the OpenAI-compatible chat/completions request shape.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"` // "auto" | "required" | "none" | {"type":"function","function":{"name":...}}
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type Message struct {
	Role       string     `json:"role"` // system|user|assistant|tool
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`         // tool name when role=tool
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool message correlation
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant tool invocations
}

type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // JSON schema
}

type ToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // "function"
	Function ToolCallInvocation `json:"function"`
}

type ToolCallInvocation struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded arg object (per OpenAI)
}

// ChatResponse — OpenAI-compatible response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Chat performs a single chat/completions call. The system message + tool
// catalogue + history must be supplied by the caller; this method does not
// retain state.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// (H3) Read up to maxResponseBytes+1 so we can distinguish "exactly at
	// the cap" from "more was waiting" — silent truncation would surface
	// downstream as garbled JSON, hard to diagnose.
	const maxResponseBytes = 8 * 1024 * 1024
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(respBody)) > maxResponseBytes {
		return nil, fmt.Errorf("llm response exceeds %d bytes — provider returned more than the safety cap", maxResponseBytes)
	}
	if resp.StatusCode/100 != 2 {
		// Strip our own bearer token + any obvious provider key prefixes
		// before surfacing the body — error strings end up in audit logs
		// and the unauthenticated UI on /investigations/{id}.
		safe := sanitizeForError(respBody, c.apiKey)
		return nil, fmt.Errorf("llm http %d: %s", resp.StatusCode, snippet(safe, 512))
	}
	var out ChatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, snippet(respBody, 256))
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("llm returned no choices: %s", snippet(respBody, 256))
	}
	return &out, nil
}

func snippet(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// isHTTPSAllowed returns true for https URLs and for plaintext http URLs
// pointing at loopback (127.0.0.0/8, ::1, localhost). Anything else is
// rejected because the bearer token must not transit cleartext.
func isHTTPSAllowed(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// keyPattern matches common provider key shapes: sk-or-..., sk-ant-...,
// sk-..., or-... (OpenRouter), tokens ≥ 16 alphanum.
var keyPattern = regexp.MustCompile(`(?i)(sk-(?:or-)?(?:ant-)?[A-Za-z0-9_\-]{16,}|or-v[0-9a-f-]{16,})`)

// sanitizeForError redacts bearer tokens that providers sometimes echo back
// in 401 / 403 bodies. Our own apiKey is replaced explicitly; any other
// token-shaped string matching keyPattern is masked too.
func sanitizeForError(body []byte, apiKey string) []byte {
	out := body
	if apiKey != "" {
		out = bytes.ReplaceAll(out, []byte(apiKey), []byte("***REDACTED***"))
	}
	return keyPattern.ReplaceAll(out, []byte("***REDACTED***"))
}
