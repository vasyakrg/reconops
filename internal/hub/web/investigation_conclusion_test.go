package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// T2: a done investigation must surface its structured diagnosis prominently,
// not only as a raw-JSON panel. The card is driven by the parsed mark_done
// summary, with graceful fallback for terminals that carry no summary.

func doneWithSummary(t *testing.T, st *store.Store, id, summaryJSON string) {
	t.Helper()
	ctx := context.Background()
	payload := store.TerminalDonePayload(summaryJSON, time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)).JSON()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: id, Goal: "diagnose nginx 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	// summary_json is only persisted by FinishInvestigation, not InsertInvestigation.
	if err := st.FinishInvestigation(ctx, id, "done", payload); err != nil {
		t.Fatal(err)
	}
}

func fragmentBody(t *testing.T, srv *Server, id string) string {
	t.Helper()
	sid, _, err := srv.sessions.issue(context.Background(), "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/investigations/fragments/"+id, nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	return rw.Body.String()
}

func TestConclusionCard_RendersStructuredDiagnosis(t *testing.T) {
	srv, st := newTestServer(t)
	summary := `{
		"root_cause":"disk full on /var starved nginx worker temp files",
		"root_cause_explains":"upstream returned 502",
		"confidence":"confirmed",
		"symptoms":["upstream returned 502","slow responses"],
		"hosts_examined":["app01","app02"],
		"evidence_refs":["task-abc123"],
		"where_to_look_next":[],
		"recommended_remediation":"truncate /var/log/nginx and add logrotate"
	}`
	doneWithSummary(t, st, "inv-done-card", summary)

	body := fragmentBody(t, srv, "inv-done-card")
	for _, want := range []string{
		">Conclusion<",
		"disk full on /var starved nginx",
		`class="badge ok"`, // confirmed -> ok
		"Recommended remediation",
		"truncate /var/log/nginx",
		"Symptoms observed",
		"upstream returned 502",
		`href="#ev-task-abc123"`, // evidence ref links to the timeline row
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("conclusion card missing %q in:\n%s", want, body)
		}
	}

	// And the structured view is wired through the data builder.
	data, err := srv.investigationDetailData(context.Background(), "inv-done-card")
	if err != nil {
		t.Fatal(err)
	}
	term := data["Terminal"].(terminalPayloadView)
	if term.Summary == nil {
		t.Fatal("Terminal.Summary is nil for a done investigation with a summary")
	}
	if term.Summary.Confidence != "confirmed" || len(term.Summary.Symptoms) != 2 {
		t.Fatalf("summary not decoded: %+v", term.Summary)
	}
}

func TestConclusionCard_GracefulWhenNoSummary(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	// operator_end / budget_finalize style terminal: no structured summary.
	payload := store.NewInvestigationTerminalPayload(
		store.TerminalKindBudgetFinalize, "budget exhausted", "", false, "loop",
		time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC)).JSON()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-done-nosummary", Goal: "g", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "inv-done-nosummary", "done", payload); err != nil {
		t.Fatal(err)
	}
	data, err := srv.investigationDetailData(ctx, "inv-done-nosummary")
	if err != nil {
		t.Fatal(err)
	}
	if term := data["Terminal"].(terminalPayloadView); term.Summary != nil {
		t.Fatalf("expected nil Summary for a budget_finalize terminal, got %+v", term.Summary)
	}
	// Must still render without the Conclusion card.
	body := fragmentBody(t, srv, "inv-done-nosummary")
	if strings.Contains(body, ">Conclusion<") {
		t.Fatalf("Conclusion card rendered for a summary-less terminal:\n%s", body)
	}
}

func TestParseTerminalSummary_NilCases(t *testing.T) {
	if parseTerminalSummary(nil) != nil {
		t.Fatal("nil raw -> want nil view")
	}
	if parseTerminalSummary(json.RawMessage(`{}`)) != nil {
		t.Fatal("empty object -> want nil view (no blank card)")
	}
	if parseTerminalSummary(json.RawMessage(`not json`)) != nil {
		t.Fatal("invalid json -> want nil view")
	}
	v := parseTerminalSummary(json.RawMessage(`{"recommended_remediation":"x"}`))
	if v == nil || v.RecommendedRemediation != "x" {
		t.Fatalf("partial summary not decoded: %+v", v)
	}
}
