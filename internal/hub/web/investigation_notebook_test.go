package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/investigator"
	"github.com/vasyakrg/recon/internal/hub/store"
)

func webTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(discardWriter{}, nil)) }

// seedNotebook writes a notebook header for id under dir, matching what the
// loop does at investigation start.
func seedNotebook(t *testing.T, dir, id string) {
	t.Helper()
	nb := investigator.NewNotebook(dir, webTestLogger())
	if err := nb.Create(store.Investigation{ID: id, Goal: "g", Model: "m", CreatedBy: "op"}, 1000, 100, time.Now().UTC()); err != nil {
		t.Fatalf("seed notebook: %v", err)
	}
}

func TestExport_IncludesFindingsAbortMemoryAndNotebook(t *testing.T) {
	srv, st := newTestServer(t)
	srv.log = webTestLogger()
	ctx := context.Background()
	dir := t.TempDir()
	srv.SetArtifactDir(dir)

	id := "inv_exp01"
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: id, Goal: "why", Status: "aborted", CreatedBy: "op"}); err != nil {
		t.Fatal(err)
	}
	_ = st.AddFinding(ctx, store.Finding{ID: "f_1", InvestigationID: id, Severity: "error", Code: "DiskFull", Message: "root fs full", EvidenceJSON: `{"task_ids":["task_1"]}`})
	payload := store.NewInvestigationTerminalPayload(store.TerminalKindLLMError, "router 502", "upstream 502", true, "llm", time.Now().UTC())
	_ = st.FinishInvestigation(ctx, id, "aborted", payload.JSON())
	_ = st.AddMemory(ctx, store.InvestigationMemory{ID: "mem_1", InvestigationID: id, Kind: store.MemoryKindFinding, Content: "root fs full", EvidenceRefsJSON: `["task_1"]`})
	seedNotebook(t, dir, id)

	req := httptest.NewRequest(http.MethodGet, "/investigations/export/"+id, nil)
	rw := httptest.NewRecorder()
	srv.handleInvestigationExport(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("code %d", rw.Code)
	}
	body := rw.Body.String()
	for _, want := range []string{
		"DiskFull", "root fs full",
		"Abort reason", "router 502",
		"Durable memory", "mem_1",
		"/investigations/notebook/" + id,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q", want)
		}
	}
}

func TestNotebookEndpoint(t *testing.T) {
	srv, st := newTestServer(t)
	srv.log = webTestLogger()
	ctx := context.Background()
	dir := t.TempDir()
	srv.SetArtifactDir(dir)

	id := "inv_nb01"
	_ = st.InsertInvestigation(ctx, store.Investigation{ID: id, Goal: "g", Status: "active", CreatedBy: "op"})
	seedNotebook(t, dir, id)

	req := httptest.NewRequest(http.MethodGet, "/investigations/notebook/"+id, nil)
	rw := httptest.NewRecorder()
	srv.handleInvestigationNotebook(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("code %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "# Investigation "+id) {
		t.Fatalf("body: %s", rw.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/investigations/notebook/inv_missing", nil)
	rw2 := httptest.NewRecorder()
	srv.handleInvestigationNotebook(rw2, req2)
	if rw2.Code != http.StatusNotFound {
		t.Fatalf("missing notebook want 404 got %d", rw2.Code)
	}
}

func TestAPIGetInvestigation_NotebookAndMemory(t *testing.T) {
	srv, st := newTestServer(t)
	srv.log = webTestLogger()
	ctx := context.Background()
	dir := t.TempDir()
	srv.SetArtifactDir(dir)

	id := "inv_api01"
	_ = st.InsertInvestigation(ctx, store.Investigation{ID: id, Goal: "g", Status: "active", CreatedBy: "op"})
	_ = st.AddMemory(ctx, store.InvestigationMemory{ID: "mem_1", InvestigationID: id, Kind: store.MemoryKindFinding, Content: "c", EvidenceRefsJSON: `["t1"]`})
	_ = st.AddMemory(ctx, store.InvestigationMemory{ID: "mem_2", InvestigationID: id, Kind: store.MemoryKindContextSummary, Content: "c2", EvidenceRefsJSON: `[]`})
	seedNotebook(t, dir, id)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/investigations/"+id, nil)
	rw := httptest.NewRecorder()
	srv.apiGetInvestigation(rw, req, id)
	if rw.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rw.Code, rw.Body.String())
	}
	var v investigationView
	if err := json.Unmarshal(rw.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.NotebookPath != "/investigations/notebook/"+id {
		t.Errorf("notebook_path=%q", v.NotebookPath)
	}
	if v.MemoryCount != 2 {
		t.Errorf("memory_count=%d want 2", v.MemoryCount)
	}
}
