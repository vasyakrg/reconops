package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	srv, err := NewServer(st, nil, nil, nil,
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
