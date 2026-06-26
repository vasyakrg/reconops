package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/investigator"
	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// newTestServer boots a minimal Server suitable for exercising /api/v1 with
// no LLM, no runner, no release poller. Good enough to verify routing, auth
// middleware, scope enforcement, and read-only inventory endpoints.
func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := NewServer(st, nil, nil, NewInvestigatorAvailability(nil, "api key missing"), nil,
		AuthConfig{}, InstallConfig{},
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st
}

type discardWriter struct{}

func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }

func issuePAT(t *testing.T, st *store.Store, scope store.APITokenScope) string {
	t.Helper()
	raw, hash, prefix, err := store.GenerateAPIToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if _, err := st.InsertAPIToken(context.Background(), "test", hash, prefix,
		string(scope), "test", sql.NullTime{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return raw
}

func TestAPI_NoBearer_401(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hosts", nil)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body=%s", rw.Code, rw.Body.String())
	}
}

func TestAPI_InvalidBearer_401(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hosts", nil)
	req.Header.Set("Authorization", "Bearer recon_pat_notarealtoken")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rw.Code)
	}
}

func TestAPI_ReadScope_ListHosts_200(t *testing.T) {
	srv, st := newTestServer(t)
	raw := issuePAT(t, st, store.APIScopeRead)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hosts", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["hosts"]; !ok {
		t.Fatalf("missing hosts key: %v", body)
	}
}

func TestAPI_ReadScope_CannotStartInvestigation_403(t *testing.T) {
	srv, st := newTestServer(t)
	raw := issuePAT(t, st, store.APIScopeRead)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/investigations",
		strings.NewReader(`{"goal":"test"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%s", rw.Code, rw.Body.String())
	}
}

func TestAPI_InvestigateScope_NoLLM_503(t *testing.T) {
	srv, st := newTestServer(t)
	raw := issuePAT(t, st, store.APIScopeInvestigate)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/investigations",
		strings.NewReader(`{"goal":"test"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 (no LLM), got %d body=%s", rw.Code, rw.Body.String())
	}
}

func TestAPI_RevokedToken_401(t *testing.T) {
	srv, st := newTestServer(t)
	raw, hash, prefix, _ := store.GenerateAPIToken()
	id, err := st.InsertAPIToken(context.Background(), "x", hash, prefix,
		string(store.APIScopeRead), "t", sql.NullTime{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeAPIToken(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hosts", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 after revoke, got %d", rw.Code)
	}
}

func TestInvestigationFragmentsRenderTimelineFindingsAndCSRF(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-live", Goal: "diagnose nginx 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-live", InvestigationID: "inv-live", Seq: 1,
		Tool: "collect", InputJSON: `{"host_id":"app01"}`, Rationale: "check nginx logs", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFinding(ctx, store.Finding{
		ID: "f-live", InvestigationID: "inv-live", Severity: "warn",
		Code: "nginx.bad_gateway", Message: "upstream returned 502",
	}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/investigations/fragments/inv-live", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{
		`id="investigation-status-fragment"`,
		`id="investigation-timeline-fragment"`,
		`id="investigation-side-fragment"`,
		`name="tool_call_id" value="tc-live"`,
		`nginx.bad_gateway`,
		`name="csrf" value="` + csrf + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("fragment missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `<html`) || strings.Contains(body, `data-active=`) {
		t.Fatalf("fragment looks like a full page render:\n%s", body)
	}
}

func TestInvestigationFragmentsRenderAbortedDisabledPanel(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-aborted", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "inv-aborted", "aborted", `{"error":"llm http 502"}`); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/investigations/fragments/inv-aborted", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{
		`Aborted reason`,
		`llm http 502`,
		`raw terminal JSON`,
		`Aborted — LLM unavailable`,
		`RECON_LLM_API_KEY`,
		`RECON_LLM_BASE_URL`,
		`RECON_LLM_MODEL`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("disabled aborted fragment missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `action="/investigations/continue"`) {
		t.Fatalf("disabled aborted fragment should not render continue form:\n%s", body)
	}
	if strings.Contains(body, `action="/investigations/hypothesis"`) {
		t.Fatalf("aborted fragment should render recovery form, not active hypothesis form:\n%s", body)
	}
}

func TestInvestigationFragmentsRenderAbortedContinueFormWhenEnabled(t *testing.T) {
	srv, st := newTestServer(t)
	srv.availability = InvestigatorAvailability{Enabled: true}
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-aborted-enabled", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "inv-aborted-enabled", "aborted", `{"error":"llm http 502"}`); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/investigations/fragments/inv-aborted-enabled", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{
		`Aborted reason`,
		`llm http 502`,
		`action="/investigations/continue"`,
		`name="message"`,
		`Address this abort reason first: llm http 502`,
		`Continue investigation`,
		`name="csrf" value="` + csrf + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("enabled aborted fragment missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `Aborted — LLM unavailable`) {
		t.Fatalf("enabled aborted fragment should not render disabled panel:\n%s", body)
	}
}

// A transient LLM abort (the screenshot scenario: 503 degraded mode) must render
// ONE recommended primary action — Retry — with "continue" demoted under a
// disclosure, never two competing primary buttons. Retry must also precede the
// continue form in source order (primary above the demoted secondary).
func TestInvestigationFragmentsRenderTransientRetryAsPrimary(t *testing.T) {
	srv, st := newTestServer(t)
	srv.availability = InvestigatorAvailability{Enabled: true}
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-transient", Goal: "diagnose 503", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	payload := store.NewInvestigationTerminalPayload(
		store.TerminalKindLLMError, "llm http 503: degraded mode", "no accounts", true, "llm", time.Now().UTC())
	payload.Transient = true
	if err := st.FinishInvestigation(ctx, "inv-transient", "aborted", payload.JSON()); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/investigations/fragments/inv-transient", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{
		`action="/investigations/retry"`,
		`Retry last step`,
		`Recommended`,
		`or steer it instead`,                  // continue is demoted under a disclosure
		`action="/investigations/continue"`,    // continue is still reachable
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("transient aborted panel missing %q in:\n%s", want, body)
		}
	}
	if strings.Index(body, "Retry last step") > strings.Index(body, `action="/investigations/continue"`) {
		t.Fatalf("retry (primary) must render before the demoted continue form")
	}
}

func TestBrowserSnapshotChangesWhenTerminalSummaryChanges(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-sse-terminal", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	before, err := srv.snapshotForSSE(ctx, "inv-sse-terminal")
	if err != nil {
		t.Fatal(err)
	}
	payload := store.NewInvestigationTerminalPayload(
		store.TerminalKindLLMError,
		"llm http 502",
		"provider returned 502",
		true,
		"llm",
		time.Now().UTC(),
	)
	if err := st.FinishInvestigation(ctx, "inv-sse-terminal", "aborted", payload.JSON()); err != nil {
		t.Fatal(err)
	}
	after, err := srv.snapshotForSSE(ctx, "inv-sse-terminal")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatalf("snapshot should change after terminal payload update: %s", after)
	}
	if !strings.Contains(after, `"terminal":"`) {
		t.Fatalf("snapshot missing terminal hash: %s", after)
	}
	if strings.Contains(after, "llm http 502") || strings.Contains(after, "provider returned 502") {
		t.Fatalf("snapshot leaked raw terminal payload: %s", after)
	}
}

func TestBrowserContinueDisabledRedirectsWithFlash(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-disabled-post", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "inv-disabled-post", "aborted", `{"error":"llm http 502"}`); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	form := strings.NewReader("csrf=" + csrf + "&investigation_id=inv-disabled-post&message=retry")
	req := httptest.NewRequest(http.MethodPost, "/investigations/continue", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	req.AddCookie(&http.Cookie{Name: cookieCSRF, Value: csrf})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("want redirect, got %d body=%s", rw.Code, rw.Body.String())
	}
	if loc := rw.Header().Get("Location"); loc != "/investigations/inv-disabled-post" {
		t.Fatalf("want redirect to detail, got %q", loc)
	}
	if strings.Contains(rw.Body.String(), "investigator disabled") {
		t.Fatalf("POST should not return plaintext disabled body: %s", rw.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/investigations/inv-disabled-post", nil)
	get.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, get)
	if rw.Code != http.StatusOK {
		t.Fatalf("want detail 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{"Continue blocked", "Continuation requires a configured LLM client", "Aborted — LLM unavailable"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail after disabled continue missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `action="/investigations/continue"`) {
		t.Fatalf("disabled detail should not render usable continue form:\n%s", body)
	}
}

func TestAPIContinueDisabledReturnsStructured503(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-api-disabled", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "inv-api-disabled", "aborted", `{"error":"llm http 502"}`); err != nil {
		t.Fatal(err)
	}
	raw := issuePAT(t, st, store.APIScopeInvestigate)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/investigations/inv-api-disabled/continue",
		strings.NewReader(`{"message":"retry"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{`"error"`, `investigator disabled`, `RECON_LLM_API_KEY`} {
		if !strings.Contains(body, want) {
			t.Fatalf("api disabled response missing %q in:\n%s", want, body)
		}
	}
}

func TestAPIGetInvestigationIncludesTerminalFields(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-api-terminal", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	payload := store.NewInvestigationTerminalPayload(
		store.TerminalKindLLMError,
		"llm http 502",
		"provider returned 502",
		true,
		"llm",
		time.Now().UTC(),
	)
	if err := st.FinishInvestigation(ctx, "inv-api-terminal", "aborted", payload.JSON()); err != nil {
		t.Fatal(err)
	}
	raw := issuePAT(t, st, store.APIScopeRead)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/investigations/inv-api-terminal", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{
		`"summary_json"`,
		`"terminal_kind": "llm_error"`,
		`"terminal_reason": "llm http 502"`,
		`"terminal_recoverable": true`,
		`"terminal_source": "llm"`,
		`"terminal_detail": "provider returned 502"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("api terminal response missing %q in:\n%s", want, body)
		}
	}
}

func TestInvestigationExportIncludesAbortReason(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-export-terminal", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	payload := store.NewInvestigationTerminalPayload(
		store.TerminalKindLLMError,
		"llm http 502",
		"provider returned 502",
		true,
		"llm",
		time.Now().UTC(),
	)
	if err := st.FinishInvestigation(ctx, "inv-export-terminal", "aborted", payload.JSON()); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/investigations/export/inv-export-terminal", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{
		`## Abort reason`,
		"- **Kind:** `llm_error`",
		"- **Reason:** llm http 502",
		"- **Recoverable:** `true`",
		"- **Source:** `llm`",
		"- **Detail:** provider returned 502",
		`## Summary`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("export missing %q in:\n%s", want, body)
		}
	}
}

func TestAPIContinueWithRealLoopCallsFakeLLM(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var seenAuth string
	fakeLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected LLM path: %s", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"test-model",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"The operator resumed after a transient router failure; no more probes are needed.",
					"tool_calls":[{
						"id":"call_resume_done",
						"type":"function",
						"function":{
							"name":"mark_done",
							"arguments":"{\"root_cause\":\"Transient upstream router failure interrupted the prior investigation.\",\"impact\":\"Investigation was aborted before final summary.\",\"evidence\":[\"operator resumed after transient 502\"],\"recommended_remediation\":\"Retry through the router after confirming LLM readiness.\",\"where_to_look_next\":[\"hub startup logs: LLM client ready\"]}"
						}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}
		}`))
	}))
	t.Cleanup(fakeLLM.Close)

	llmClient, err := llm.New(llm.Options{
		BaseURL:           fakeLLM.URL,
		APIKey:            "test-api-key",
		Model:             "test-model",
		AllowInsecureHTTP: true,
		Timeout:           2 * time.Second,
	})
	if err != nil {
		t.Fatalf("new llm client: %v", err)
	}
	loop := investigator.NewLoop(st, llmClient, nil, func(string) bool { return false }, func() []string { return nil }, 40, 500000,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	loop.SetBus(investigator.NewBus())
	srv, err := NewServer(st, nil, loop, NewInvestigatorAvailability(loop, ""), nil,
		AuthConfig{}, InstallConfig{},
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-real-loop", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "test-model", BaseURL: fakeLLM.URL,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, store.Message{InvestigationID: "inv-real-loop", Role: "system", Content: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, store.Message{InvestigationID: "inv-real-loop", Role: "user", Content: "goal"}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "inv-real-loop", "aborted", `{"error":"llm http 502"}`); err != nil {
		t.Fatal(err)
	}
	raw := issuePAT(t, st, store.APIScopeInvestigate)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/investigations/inv-real-loop/continue",
		strings.NewReader(`{"message":"retry now"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s investigation_id=inv-real-loop", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), `"status": "active"`) {
		t.Fatalf("response missing active status: %s", rw.Body.String())
	}
	inv, err := st.GetInvestigation(ctx, "inv-real-loop")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status != "active" || inv.SummaryJSON.Valid {
		t.Fatalf("resume did not reactivate and clear summary: %+v", inv)
	}
	msgs, err := st.ListMessages(ctx, "inv-real-loop", true)
	if err != nil {
		t.Fatal(err)
	}
	foundResume := false
	for _, msg := range msgs {
		if msg.Role == "user" && strings.Contains(msg.Content, "OPERATOR RESUME") {
			foundResume = true
			break
		}
	}
	if !foundResume {
		t.Fatalf("OPERATOR RESUME message not appended: %+v", msgs)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tc, err := st.GetToolCall(ctx, "call_resume_done"); err == nil {
			if tc.Tool != "mark_done" || tc.Status != "pending" {
				t.Fatalf("unexpected deterministic tool call: %+v", tc)
			}
			if seenAuth != "Bearer test-api-key" {
				t.Fatalf("fake LLM did not receive expected bearer auth, got %q", seenAuth)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for deterministic fake LLM tool call, investigation_id=inv-real-loop")
}

func TestInvestigationDetailNoStateChangeReloadInTemplate(t *testing.T) {
	tpl, err := os.ReadFile(filepath.Join("templates", "investigation_detail.html"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tpl), "window.location.reload") {
		t.Fatal("investigation detail template must not reload the page on SSE state-change")
	}
	js, err := os.ReadFile(filepath.Join("static", "hub.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "if (!ev.data || ev.data !== currentSnapshot) refresh();") {
		t.Fatal("state-change handler should refresh fragments instead of reloading the page")
	}
	if strings.Contains(string(js), "window.location.reload") {
		t.Fatal("live update failure path should not require a manual/full page reload")
	}
	if !strings.Contains(string(js), "setInterval(refresh, BACKSTOP_MS)") {
		t.Fatal("live investigation page should run an always-on backstop fragment poll")
	}
	// The refresh must be change-aware PER REGION (compare each region's own
	// data-frag-hash) so a volatile field in one region (the status budget bar,
	// which churns every turn) does not reflow the timeline/side on every poll
	// or heartbeat — the reported flicker. The SSE-level pre-filter still uses
	// the global snapshot (ev.data !== currentSnapshot) to avoid even fetching.
	if !strings.Contains(string(js), "if (!regionChanged && !dirty[id]) return;") {
		t.Fatal("refresh should skip a region's DOM swap when that region's own content hash is unchanged")
	}
	// The initial snapshot must be read from the rendered data-snapshot
	// attribute (same encoding as fetched fragments + SSE), not the inline
	// printf %q value — otherwise the first comparison mismatches and swaps.
	if !strings.Contains(string(js), "initStatus.getAttribute('data-snapshot')") {
		t.Fatal("currentSnapshot must be sourced from the rendered data-snapshot attribute, not the inline opts value")
	}
	if !strings.Contains(string(js), "scheduleReconnect();") {
		t.Fatal("bye handler should reconnect instead of leaving long-running investigation pages stale")
	}
	if !strings.Contains(string(js), "[FIX:investigation-live] ") || !strings.Contains(string(js), "connecting event stream") {
		t.Fatal("live reconnect path should include [FIX:investigation-live] diagnostics")
	}
	layout, err := os.ReadFile(filepath.Join("templates", "layout.html"))
	if err != nil {
		t.Fatal(err)
	}
	hubScript := strings.Index(string(layout), `<script src="/static/hub.js?v=`)
	content := strings.Index(string(layout), `{{template "content" .}}`)
	if hubScript < 0 || content < 0 || hubScript > content {
		t.Fatal("hub.js must load before page content so investigation inline startup can call ReconInvestigationLive")
	}
	server, err := os.ReadFile(filepath.Join("server.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(server), "EnsureProgress(ctx, id,") < 2 {
		t.Fatal("investigation detail and SSE snapshot paths should nudge active investigations")
	}
}
