package web

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/investigator"
	"github.com/vasyakrg/recon/internal/hub/store"
)

func TestPriorChoicesFrom(t *testing.T) {
	ps := []store.PriorInvestigation{
		{
			ID: "a", Goal: "g", CreatedAt: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC),
			AllowedHosts: []string{"h1", "h2"},
			SummaryJSON:  sql.NullString{String: store.TerminalDonePayload(`{"root_cause":"disk full"}`, time.Time{}).JSON(), Valid: true},
		},
		{ID: "b", Goal: "g2", AllowedHosts: nil, SummaryJSON: sql.NullString{}},
	}
	got := priorChoicesFrom(ps)
	if len(got) != 2 {
		t.Fatalf("want 2 choices, got %d", len(got))
	}
	if got[0].ID != "a" || got[0].RootCause != "disk full" || got[0].Hosts != "h1, h2" || got[0].Date != "2026-06-20" {
		t.Fatalf("mapping wrong: %+v", got[0])
	}
	if got[1].Hosts != "all hosts" || got[1].RootCause != "" {
		t.Fatalf("empty/legacy prior mapping wrong: %+v", got[1])
	}
}

// The priors panel must surface the FULL conclusion (the operator complained the
// 240-char terminal stub hid the rest), with confidence, and still fall back to
// the one-line reason for legacy payloads that carry no structured summary.
func TestPriorChoicesFromFullRootCause(t *testing.T) {
	long := strings.Repeat("very long detail ", 40) // ~680 chars, well past the 240 cap
	full := `{"root_cause":"` + long + `","confidence":"likely"}`
	ps := []store.PriorInvestigation{
		{ID: "full", SummaryJSON: sql.NullString{String: store.TerminalDonePayload(full, time.Time{}).JSON(), Valid: true}},
		{ID: "legacy", SummaryJSON: sql.NullString{String: `{"kind":"done","reason":"Investigation complete: short legacy reason"}`, Valid: true}},
	}
	got := priorChoicesFrom(ps)
	if len(got) != 2 {
		t.Fatalf("want 2 choices, got %d", len(got))
	}
	if got[0].RootCause != strings.TrimSpace(long) {
		t.Fatalf("want full untruncated root_cause, got %q", got[0].RootCause)
	}
	if len(got[0].RootCause) <= 240 {
		t.Fatalf("root_cause must NOT be truncated to the terminal cap, got %d chars", len(got[0].RootCause))
	}
	if got[0].Confidence != "likely" {
		t.Fatalf("want confidence 'likely', got %q", got[0].Confidence)
	}
	if got[1].RootCause != "short legacy reason" {
		t.Fatalf("legacy prior must fall back to the reason line, got %q", got[1].RootCause)
	}
}

// The new-investigation form must offer a prior_ids picker listing recent done
// investigations (id + conclusion) when priors injection is enabled.
func TestNewInvestigationFormRendersPriorCandidates(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "prior-x", Goal: "etcd flap", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u", AllowedHosts: []string{"node1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "prior-x", "done",
		store.TerminalDonePayload(`{"root_cause":"clock skew on node1"}`, time.Time{}).JSON()); err != nil {
		t.Fatal(err)
	}
	// An aborted prior must also be offered (manual picker is any-status).
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "prior-ab", Goal: "disk pressure probe", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "prior-ab", "aborted", `{"kind":"aborted"}`); err != nil {
		t.Fatal(err)
	}

	loop := investigator.NewLoop(st, nil, nil, func(string) bool { return false }, func() []string { return nil }, 40, 500000,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	loop.SetPriorsConfig(investigator.DefaultPriorsConfig()) // enabled
	srv, err := NewServer(st, nil, loop, NewInvestigatorAvailability(loop, ""), nil,
		AuthConfig{}, InstallConfig{}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/investigations", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /investigations = %d, body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, `name="prior_ids"`) {
		t.Fatalf("create form must offer a prior_ids picker checkbox")
	}
	if !strings.Contains(body, "prior-x") || !strings.Contains(body, "clock skew on node1") {
		t.Fatalf("done prior candidate (id + conclusion) must render in the form")
	}
	// The aborted run is offered too, with its status badge + goal.
	if !strings.Contains(body, "prior-ab") || !strings.Contains(body, "disk pressure probe") {
		t.Fatalf("aborted prior candidate must also render in the form")
	}
	if !strings.Contains(body, ">aborted<") {
		t.Fatalf("candidate must show a status badge")
	}
}

// The detail page must surface which prior investigations were attached to a
// run (the visibility gap), with a link to each prior and its conclusion; a run
// with no attached priors renders no panel.
func TestDetailRendersAttachedPriorsPanel(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "prior-y", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u", AllowedHosts: []string{"h9"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "prior-y", "done",
		store.TerminalDonePayload(`{"root_cause":"bad NIC firmware"}`, time.Time{}).JSON()); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-cur", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetInvestigationPriors(ctx, "inv-cur", []string{"prior-y"}); err != nil {
		t.Fatal(err)
	}

	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := fetchFragment(t, srv, sid, "inv-cur")
	if !strings.Contains(body, "Prior investigations") {
		t.Fatalf("detail must render the Prior investigations panel")
	}
	if !strings.Contains(body, `href="/investigations/prior-y"`) || !strings.Contains(body, "bad NIC firmware") {
		t.Fatalf("priors panel must link the prior and show its conclusion")
	}

	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-none", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if body2 := fetchFragment(t, srv, sid, "inv-none"); strings.Contains(body2, "Prior investigations") {
		t.Fatalf("a run with no attached priors must not render the panel")
	}
}
