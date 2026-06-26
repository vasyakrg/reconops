package investigator

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

func nbTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func readNotebook(t *testing.T, root, id string) string {
	t.Helper()
	p, err := NotebookPath(root, id)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestInvestigationArtifactDir_Traversal(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`, "/abs", "inv/../../etc"} {
		if _, err := InvestigationArtifactDir(root, id); err == nil {
			t.Errorf("expected error for id %q", id)
		}
	}
	if _, err := InvestigationArtifactDir("", "inv_x"); err == nil {
		t.Error("expected error for empty root")
	}
	dir, err := InvestigationArtifactDir(root, "inv_abc123")
	if err != nil {
		t.Fatalf("valid id: %v", err)
	}
	if want := filepath.Join(root, "investigations", "inv_abc123"); dir != want {
		t.Fatalf("got %q want %q", dir, want)
	}
}

func TestNotebookLifecycle(t *testing.T) {
	root := t.TempDir()
	nb := NewNotebook(root, nbTestLogger())
	id := "inv_test01"
	inv := store.Investigation{ID: id, Goal: "why cron fails", Model: "test/model", CreatedBy: "op", AllowedHosts: []string{"h1"}}
	if err := nb.Create(inv, 200000, 4096, time.Now().UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second Create (e.g. on resume) must not clobber the header.
	if err := nb.Create(inv, 1, 1, time.Now().UTC()); err != nil {
		t.Fatalf("create2: %v", err)
	}
	_ = nb.AppendFinding(id, store.Finding{ID: "f_1", Severity: "error", Code: "DiskFull", Message: "root fs at 100%"}, []string{"task_1", "task_2"}, "mem_1")
	_ = nb.AppendMemory(id, store.InvestigationMemory{ID: "mem_2", Kind: store.MemoryKindContextSummary, Content: "summary text", MessageSeqStart: 3, MessageSeqEnd: 9, TokenEstimate: 42})
	_ = nb.AppendOperatorHypothesis(id, "etcd is the cause", "high latency", "probe etcd")
	_ = nb.AppendAbort(id, store.NewInvestigationTerminalPayload(store.TerminalKindLLMError, "router 502", "upstream 502", true, "llm", time.Now().UTC()))
	_ = nb.AppendMarkDone(id, `{"root_cause":"disk full on h1","symptoms":["oom"]}`)

	body := readNotebook(t, root, id)
	for _, want := range []string{
		"# Investigation inv_test01", "why cron fails", "Context window (tokens):** 200000",
		"f_1", "DiskFull", "task_1, task_2", "mem_1",
		`<a id="memory-mem_2"`, "summary text",
		"Operator hypothesis", "etcd is the cause",
		"Aborted", "router 502",
		"Conclusion (mark_done)", "disk full on h1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notebook missing %q", want)
		}
	}
	if n := strings.Count(body, "# Investigation inv_test01"); n != 1 {
		t.Errorf("header written %d times, want 1", n)
	}
}

// T3: across a reopen→reclose the model can accept mark_done more than once.
// The notebook must keep a SINGLE "## Conclusion (mark_done)" section (the
// stack of 11 in inv_a00000000005 was the bug); the live latest stays in
// summary_json + the web Conclusion card.
func TestNotebookMarkDoneDedupAcrossReclose(t *testing.T) {
	root := t.TempDir()
	nb := NewNotebook(root, nbTestLogger())
	id := "inv_reclose"
	inv := store.Investigation{ID: id, Goal: "g", Model: "m", CreatedBy: "op"}
	if err := nb.Create(inv, 1000, 100, time.Now().UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := nb.AppendMarkDone(id, `{"root_cause":"first cause","confidence":"likely"}`); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Reopen→reclose: a second accepted mark_done must NOT stack another section.
	if err := nb.AppendMarkDone(id, `{"root_cause":"second cause","confidence":"confirmed"}`); err != nil {
		t.Fatalf("second close: %v", err)
	}

	body := readNotebook(t, root, id)
	if n := strings.Count(body, "## Conclusion (mark_done)"); n != 1 {
		t.Fatalf("conclusion section written %d times, want exactly 1:\n%s", n, body)
	}
	if n := strings.Count(body, `<a id="done"></a>`); n != 1 {
		t.Fatalf("done anchor written %d times, want 1", n)
	}
	// First close preserved (append-only, no in-place rewrite); the repeat dropped.
	if !strings.Contains(body, "first cause") {
		t.Fatalf("expected the first conclusion to remain:\n%s", body)
	}
	if strings.Contains(body, "second cause") {
		t.Fatalf("second conclusion should have been suppressed:\n%s", body)
	}
}

func TestNotebookDisabled(t *testing.T) {
	nb := NewNotebook("", nbTestLogger())
	if nb.enabled() {
		t.Fatal("empty-root notebook should be disabled")
	}
	if err := nb.AppendFinding("inv_x", store.Finding{ID: "f"}, nil, ""); err != nil {
		t.Fatalf("disabled append should be a nil-op: %v", err)
	}
	if _, ok := nb.Path("inv_x"); ok {
		t.Fatal("disabled notebook path should be unavailable")
	}
}

func TestRenderMemoryDigest(t *testing.T) {
	if RenderMemoryDigest(nil) != "" {
		t.Fatal("empty input should render empty")
	}
	out := RenderMemoryDigest([]store.InvestigationMemory{
		{ID: "mem_1", Kind: "finding", Content: "disk full"},
	})
	if !strings.Contains(out, "mem_1") || !strings.Contains(out, "finding") || !strings.Contains(out, "disk full") {
		t.Fatalf("digest missing fields: %q", out)
	}
}
