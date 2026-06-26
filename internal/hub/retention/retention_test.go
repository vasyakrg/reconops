package retention

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

func backdateInvestigation(t *testing.T, st *store.Store, id string, days int) {
	t.Helper()
	ts := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UTC()
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE investigations SET updated_at=? WHERE id=?`, ts, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if got := err == nil; got != want {
		t.Errorf("exists(%s)=%v want %v", path, got, want)
	}
}

func TestCleanupInvestigationArtifacts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	root := filepath.Join(dir, "artifacts")
	invRoot := filepath.Join(root, "investigations")
	mk := func(id string) string {
		d := filepath.Join(invRoot, id)
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "notebook.md"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// active → keep
	_ = st.InsertInvestigation(ctx, store.Investigation{ID: "inv_active", Goal: "g", Status: "active", CreatedBy: "op"})
	dActive := mk("inv_active")
	// done + expired → remove
	_ = st.InsertInvestigation(ctx, store.Investigation{ID: "inv_old", Goal: "g", Status: "done", CreatedBy: "op"})
	_ = st.FinishInvestigation(ctx, "inv_old", "done", "")
	backdateInvestigation(t, st, "inv_old", 60)
	dOld := mk("inv_old")
	// done + recent → keep
	_ = st.InsertInvestigation(ctx, store.Investigation{ID: "inv_recent", Goal: "g", Status: "done", CreatedBy: "op"})
	_ = st.FinishInvestigation(ctx, "inv_recent", "done", "")
	dRecent := mk("inv_recent")
	// orphan (no investigation row) + old mtime → remove
	dOrphan := mk("inv_orphan")
	old := time.Now().Add(-60 * 24 * time.Hour)
	_ = os.Chtimes(dOrphan, old, old)

	// a normal orphan task dir at the artifact root (old) must be cleaned by
	// task cleanup, while the investigations subtree must be left untouched.
	taskDir := filepath.Join(root, "task_orphan")
	if err := os.MkdirAll(taskDir, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(taskDir, old, old)

	w := New(st, root, 30, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.sweep(ctx)

	assertExists(t, dActive, true)
	assertExists(t, dOld, false)
	assertExists(t, dRecent, true)
	assertExists(t, dOrphan, false)
	assertExists(t, invRoot, true)  // investigations subtree survived task cleanup
	assertExists(t, taskDir, false) // normal orphan task dir was cleaned
}
