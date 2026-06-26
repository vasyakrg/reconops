package investigator

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// TestExecuteApprovedAddFindingDurable verifies the Task 8 durability path
// end to end: a successful add_finding writes the findings row, a kind=finding
// memory record carrying the evidence task_ids, a notebook section, and a
// system_note instructing the model to cite the ids.
func TestExecuteApprovedAddFindingDurable(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const inv = "inv-durable"
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: inv, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRun(ctx, store.Run{ID: "run1", InvestigationID: sql.NullString{String: inv, Valid: true}, Name: "c", CreatedBy: "o", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHost(ctx, store.Host{ID: "h1", Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "fp", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"task-a", "task-b"} {
		if err := st.InsertTask(ctx, store.Task{ID: id, RunID: "run1", HostID: "h1", Collector: "system_info", Status: "done"}); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	l := &Loop{store: st, bus: NewBus()}
	l.SetArtifactDir(dir)

	args := `{"severity":"error","code":"disk.full","message":"root fs at 100% on h1","evidence_refs":["task-a","task-b"]}`
	tc := store.ToolCallRow{ID: "tc1", InvestigationID: inv, Seq: 1, Tool: "add_finding", InputJSON: args, Status: "approved"}
	if err := st.InsertToolCall(ctx, tc); err != nil {
		t.Fatal(err)
	}
	if err := l.executeApproved(ctx, inv, &tc); err != nil {
		t.Fatal(err)
	}

	findings, _ := st.ListFindings(ctx, inv)
	if len(findings) != 1 || findings[0].Code != "disk.full" {
		t.Fatalf("findings: %+v", findings)
	}
	mems, _ := st.ListMemory(ctx, inv, 10)
	if len(mems) != 1 || mems[0].Kind != store.MemoryKindFinding {
		t.Fatalf("memory: %+v", mems)
	}
	if !strings.Contains(mems[0].EvidenceRefsJSON, "task-a") || !strings.Contains(mems[0].EvidenceRefsJSON, "task-b") {
		t.Fatalf("memory evidence refs: %s", mems[0].EvidenceRefsJSON)
	}

	nbPath, _ := NotebookPath(dir, inv)
	body, err := os.ReadFile(nbPath)
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	for _, want := range []string{"disk.full", "root fs at 100% on h1", "task-a, task-b"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("notebook missing %q", want)
		}
	}

	msgs, _ := st.ListMessages(ctx, inv, true)
	foundNote := false
	for _, m := range msgs {
		if m.Role == "system_note" && strings.Contains(m.Content, "stored durably") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatal("expected a durability system_note citing the stored ids")
	}
}
