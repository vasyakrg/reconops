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
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// Outbound-proxy env vars route ALL LLM-provider traffic (chat/completions +
// GET /models) through a forward proxy. The proxy is configured as SEPARATE
// fields — never a single compound URL — so the operator does not hand-escape
// credentials or glue scheme+host together. Empty TYPE → direct connection.
//
//	RECON_LLM_PROXY_TYPE  http | https | socks5 | socks5h   (empty disables)
//	RECON_LLM_PROXY_ADDR  host:port
//	RECON_LLM_PROXY_USER  optional proxy username
//	RECON_LLM_PROXY_PASS  optional proxy password
//
// http/https use an HTTP CONNECT proxy (proxy resolves DNS). socks5/socks5h use
// SOCKS5 with the target hostname handed to the proxy, so DNS resolves on the
// proxy side (DNS-over-SOCKS) — what reaches a geo-/network-blocked provider
// endpoint from a host whose resolver/route cannot.
const (
	envProxyType = "RECON_LLM_PROXY_TYPE"
	envProxyAddr = "RECON_LLM_PROXY_ADDR"
	envProxyUser = "RECON_LLM_PROXY_USER"
	envProxyPass = "RECON_LLM_PROXY_PASS"
	// envTimeout overrides the per-request LLM HTTP timeout (defaultTimeout).
	// Slow/free reasoning models can exceed 120s on a full max_tokens
	// generation; raise this so the investigator step completes instead of
	// tripping "read response: context deadline exceeded".
	envTimeout = "RECON_LLM_TIMEOUT"
)

// proxyURLFromEnv assembles the internal proxy URL from the separate
// RECON_LLM_PROXY_* fields, or "" when RECON_LLM_PROXY_TYPE is unset. Keeping
// the parts separate means the password is taken verbatim from its own env var
// (no manual URL-escaping) and the operator never writes a URL by hand.
func proxyURLFromEnv() (string, error) {
	typ := strings.ToLower(strings.TrimSpace(os.Getenv(envProxyType)))
	if typ == "" {
		return "", nil
	}
	switch typ {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("llm: %s %q unsupported (use http, https, socks5, or socks5h)", envProxyType, typ)
	}
	addr := strings.TrimSpace(os.Getenv(envProxyAddr))
	if addr == "" {
		return "", fmt.Errorf("llm: %s=%s is set but %s (host:port) is empty", envProxyType, typ, envProxyAddr)
	}
	u := url.URL{Scheme: typ, Host: addr}
	if user := strings.TrimSpace(os.Getenv(envProxyUser)); user != "" {
		if pass, ok := os.LookupEnv(envProxyPass); ok {
			u.User = url.UserPassword(user, pass)
		} else {
			u.User = url.User(user)
		}
	}
	return u.String(), nil
}

// timeoutFromEnv reads RECON_LLM_TIMEOUT, the per-request LLM HTTP timeout.
// It accepts a Go duration string ("180s", "3m", "2m30s") or a bare integer of
// seconds ("180"). Empty → 0, so New falls back to defaultTimeout. A value that
// parses but is non-positive is a hard error, not a silent fallback, so a typo
// surfaces at startup instead of behaving as "no timeout was meant".
func timeoutFromEnv() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(envTimeout))
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		n, ierr := strconv.Atoi(raw)
		if ierr != nil {
			return 0, fmt.Errorf("llm: %s %q invalid (use a duration like 180s/3m or integer seconds)", envTimeout, raw)
		}
		d = time.Duration(n) * time.Second
	}
	if d <= 0 {
		return 0, fmt.Errorf("llm: %s %q must be positive", envTimeout, raw)
	}
	return d, nil
}

const defaultTimeout = 120 * time.Second

type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
	headers    map[string]string
}

type Options struct {
	BaseURL           string
	APIKey            string
	Model             string
	AllowInsecureHTTP bool
	HTTPReferer       string // OpenRouter ranking; harmless on other backends
	XTitle            string // OpenRouter ranking; harmless on other backends
	Timeout           time.Duration
	// Proxy, when non-empty, routes outbound provider traffic through a
	// forward proxy (http/https CONNECT or socks5/socks5h). See envProxy for
	// the accepted forms. Empty → direct connection.
	Proxy string
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
	if err := validateBaseURLTransport(opt.BaseURL, opt.AllowInsecureHTTP); err != nil {
		return nil, err
	}
	to := opt.Timeout
	if to <= 0 {
		to = defaultTimeout
	}
	httpClient, err := newHTTPClient(to, opt.Proxy)
	if err != nil {
		return nil, err
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
		httpClient: httpClient,
		headers:    headers,
	}, nil
}

// newHTTPClient builds the outbound HTTP client, optionally routing through a
// forward proxy. An empty proxyURL yields the previous behavior (a plain
// timeout-bounded client on the default transport). For socks5/socks5h the
// dialer hands the target hostname to the proxy, so DNS is resolved on the
// proxy side (DNS-over-SOCKS) — the configuration needed to reach a provider
// the local network/resolver cannot.
func newHTTPClient(timeout time.Duration, proxyURL string) (*http.Client, error) {
	c := &http.Client{Timeout: timeout}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return c, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("llm: parse proxy %q: %w", proxyURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("llm: proxy %q has no host:port", proxyURL)
	}
	// Clone the default transport so keep-alive, idle-pool and TLS defaults are
	// preserved; we only swap how connections are established.
	tr := defaultTransportClone()
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, derr := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if derr != nil {
			return nil, fmt.Errorf("llm: build SOCKS5 dialer for proxy %q: %w", proxyURL, derr)
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("llm: SOCKS5 dialer lacks context support")
		}
		tr.DialContext = cd.DialContext
		// A SOCKS dialer must not also honor HTTP(S)_PROXY from the environment.
		tr.Proxy = nil
	default:
		return nil, fmt.Errorf("llm: proxy scheme %q unsupported (use http, https, socks5, or socks5h)", u.Scheme)
	}
	c.Transport = tr
	return c, nil
}

// defaultTransportClone returns a fresh copy of http.DefaultTransport's
// settings, falling back to a sensible transport if the stdlib default is ever
// replaced by a non-*http.Transport value.
func defaultTransportClone() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

// NewFromEnv resolves API key from envName at the moment of construction.
func NewFromEnv(baseURL, model, envName string, allowInsecureHTTP bool, referer, title string) (*Client, error) {
	if envName == "" {
		envName = "RECON_LLM_API_KEY"
	}
	proxyURL, err := proxyURLFromEnv()
	if err != nil {
		return nil, err
	}
	timeout, err := timeoutFromEnv()
	if err != nil {
		return nil, err
	}
	return New(Options{
		BaseURL:           baseURL,
		APIKey:            os.Getenv(envName),
		Model:             model,
		AllowInsecureHTTP: allowInsecureHTTP,
		HTTPReferer:       referer,
		XTitle:            title,
		Proxy:             proxyURL,
		Timeout:           timeout,
	})
}

func (c *Client) Model() string   { return c.model }
func (c *Client) BaseURL() string { return c.baseURL }

// Timeout returns the effective per-request HTTP timeout (including the
// defaultTimeout fallback), so callers can log what is actually in force.
func (c *Client) Timeout() time.Duration { return c.httpClient.Timeout }

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
	// CacheControl, when true, marks this message as a prompt-cache breakpoint
	// (Task 4). It is wire-only state, never serialized as its own field: see
	// MarshalJSON. The caller sets it ONLY for cache-capable routes, so the
	// disabled path stays byte-identical to plain OpenAI chat messages.
	CacheControl bool `json:"-"`
}

// cacheControlMarker is the Anthropic-style ephemeral cache breakpoint emitted
// inside a content block when CacheControl is set.
type cacheControlMarker struct {
	Type string `json:"type"`
}

type cacheTextBlock struct {
	Type         string             `json:"type"`
	Text         string             `json:"text"`
	CacheControl cacheControlMarker `json:"cache_control"`
}

// MarshalJSON keeps the default wire shape (content as a plain string) for every
// message EXCEPT a cache-breakpoint message with non-empty content, which is
// emitted as a single text content block carrying cache_control:{type:ephemeral}
// — the form OpenRouter forwards to Anthropic-family models for prompt caching.
// OpenAI/vLLM and non-cache routes never set CacheControl, so they see the
// identical bytes as before this change.
func (m Message) MarshalJSON() ([]byte, error) {
	type alias Message // no MarshalJSON method → default struct-tag encoding
	if !m.CacheControl || m.Content == "" {
		return json.Marshal(alias(m))
	}
	return json.Marshal(struct {
		Role       string           `json:"role"`
		Content    []cacheTextBlock `json:"content"`
		Name       string           `json:"name,omitempty"`
		ToolCallID string           `json:"tool_call_id,omitempty"`
		ToolCalls  []ToolCall       `json:"tool_calls,omitempty"`
	}{
		Role: m.Role,
		Content: []cacheTextBlock{{
			Type:         "text",
			Text:         m.Content,
			CacheControl: cacheControlMarker{Type: "ephemeral"},
		}},
		Name:       m.Name,
		ToolCallID: m.ToolCallID,
		ToolCalls:  m.ToolCalls,
	})
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
	// PromptTokensDetails is optionally returned by cache-capable providers
	// (OpenAI / OpenRouter) and reports how many prompt tokens were served
	// from cache. Absent on providers that don't support it — decoding stays
	// backward-compatible (Task 15).
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// CachedTokens returns the cached prompt-token count, or 0 when the provider
// did not report cache usage.
func (u Usage) CachedTokens() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
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
		return nil, c.providerError(ProviderErrorUnknown, 0, err.Error(), false)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, c.providerError(ProviderErrorNetwork, 0, err.Error(), true)
	}
	defer func() { _ = resp.Body.Close() }()

	// (H3) Read up to maxResponseBytes+1 so we can distinguish "exactly at
	// the cap" from "more was waiting" — silent truncation would surface
	// downstream as garbled JSON, hard to diagnose.
	const maxResponseBytes = 8 * 1024 * 1024
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, c.providerError(ProviderErrorNetwork, 0, "read response: "+err.Error(), true)
	}
	if int64(len(respBody)) > maxResponseBytes {
		return nil, c.providerError(ProviderErrorDecode, 0,
			fmt.Sprintf("llm response exceeds %d bytes — provider returned more than the safety cap", maxResponseBytes), false)
	}
	if resp.StatusCode/100 != 2 {
		// Strip our own bearer token + any obvious provider key prefixes
		// before surfacing the body — error strings end up in audit logs
		// and the unauthenticated UI on /investigations/{id}.
		safe := sanitizeForError(respBody, c.apiKey)
		return nil, c.providerError(classifyHTTPProviderError(resp.StatusCode, safe), resp.StatusCode, snippet(safe, 512), isRetryableHTTPStatus(resp.StatusCode))
	}
	var out ChatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		// A 2xx body we can't parse is almost always a transient gateway
		// hiccup (a flaky load balancer emitting an error page or a truncated
		// body), not a permanent incompatibility — mark retryable so the
		// router retries before aborting the investigation. If it's genuinely
		// incompatible, the retries exhaust and the abort still reports decode.
		safe := sanitizeForError(respBody, c.apiKey)
		return nil, c.providerError(ProviderErrorDecode, 0,
			fmt.Sprintf("decode response: %v (body: %s)", err, snippet(safe, 256)), true)
	}
	if len(out.Choices) == 0 {
		// Empty choices from a 2xx is a transient model/gateway hiccup; retry
		// rather than abort.
		safe := sanitizeForError(respBody, c.apiKey)
		return nil, c.providerError(ProviderErrorDecode, 0, "llm returned no choices: "+snippet(safe, 256), true)
	}
	return &out, nil
}

// ModelInfo is a lenient view of one entry from an OpenAI-compatible
// GET /models response. The context window is exposed under different keys per
// backend (OpenRouter: context_length; vLLM: max_model_len); raw OpenAI exposes
// none, so both may be absent.
type ModelInfo struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	MaxModelLen   int    `json:"max_model_len"`
}

// ContextWindow returns the model's context window from whichever provider
// field is populated, or 0 when the backend does not expose it.
func (m ModelInfo) ContextWindow() int {
	if m.ContextLength > 0 {
		return m.ContextLength
	}
	if m.MaxModelLen > 0 {
		return m.MaxModelLen
	}
	return 0
}

// ListModels performs a best-effort GET {base_url}/models and returns a map of
// model id -> context window for entries that expose one. It is advisory only:
// a transport / non-2xx / decode failure or an absent window field is a soft
// miss the caller is expected to ignore (keep the configured/fallback window).
// This is a pure read with no target-host state change.
func (c *Client) ListModels(ctx context.Context) (map[string]int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, c.providerError(ProviderErrorUnknown, 0, err.Error(), false)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, c.providerError(ProviderErrorNetwork, 0, err.Error(), true)
	}
	defer func() { _ = resp.Body.Close() }()
	const maxResponseBytes = 8 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, c.providerError(ProviderErrorNetwork, 0, "read response: "+err.Error(), true)
	}
	if resp.StatusCode/100 != 2 {
		safe := sanitizeForError(body, c.apiKey)
		return nil, c.providerError(classifyHTTPProviderError(resp.StatusCode, safe), resp.StatusCode, snippet(safe, 256), isRetryableHTTPStatus(resp.StatusCode))
	}
	var out struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		safe := sanitizeForError(body, c.apiKey)
		return nil, c.providerError(ProviderErrorDecode, 0, "decode models: "+snippet(safe, 256), false)
	}
	windows := map[string]int{}
	for _, m := range out.Data {
		if w := m.ContextWindow(); w > 0 && m.ID != "" {
			windows[m.ID] = w
		}
	}
	return windows, nil
}

func (c *Client) providerError(class ProviderErrorClass, statusCode int, detail string, retryable bool) *ProviderError {
	host := ""
	if u, err := url.Parse(c.baseURL); err == nil {
		host = u.Host
	}
	return NewProviderError(class, statusCode, host, detail, retryable)
}

func classifyHTTPProviderError(statusCode int, body []byte) ProviderErrorClass {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return ProviderErrorAuth
	}
	if statusCode == http.StatusTooManyRequests {
		return ProviderErrorRateLimit
	}
	if statusCode >= 500 {
		return ProviderErrorServer
	}
	bodyLower := strings.ToLower(string(body))
	if strings.Contains(bodyLower, "context") && (strings.Contains(bodyLower, "limit") || strings.Contains(bodyLower, "length") || strings.Contains(bodyLower, "window")) {
		return ProviderErrorContextLimit
	}
	return ProviderErrorUnknown
}

func isRetryableHTTPStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= 500
}

func snippet(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// validateBaseURLTransport accepts HTTPS everywhere, and accepts plaintext
// HTTP only for loopback by default. With AllowInsecureHTTP, HTTP is also
// allowed for literal private or link-local IP endpoints: RFC1918 IPv4,
// IPv4 link-local, IPv6 ULA, and IPv6 link-local. Public HTTP URLs and HTTP
// hostnames other than localhost stay rejected even with the opt-in because
// bearer tokens would otherwise transit cleartext to an ambiguous endpoint.
func validateBaseURLTransport(rawURL string, allowInsecureHTTP bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("llm: parse base_url %q: %w", rawURL, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return fmt.Errorf("llm: base_url %q uses unsupported scheme %q", rawURL, u.Scheme)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("llm: base_url %q is plaintext HTTP to a non-loopback hostname — bearer token would leak", rawURL)
	}
	if ip.IsLoopback() {
		return nil
	}
	if allowInsecureHTTP && (ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return nil
	}
	if allowInsecureHTTP {
		return fmt.Errorf("llm: base_url %q is plaintext HTTP to a public IP — bearer token would leak", rawURL)
	}
	return fmt.Errorf("llm: base_url %q is plaintext HTTP and not loopback; set RECON_LLM_ALLOW_INSECURE_HTTP=true only for private/link-local router endpoints", rawURL)
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
