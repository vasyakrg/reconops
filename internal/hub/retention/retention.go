// Package retention implements the background cleaner: it removes artifact
// directories of finished tasks older than retention_days and prunes
// archived messages from compacted investigations. Runs in a single
// goroutine; ctx-cancellable.
package retention

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// investigationsSubdir is the artifact-root subtree holding investigation-
// scoped artifacts (notebooks). It mirrors investigator.investigationsSubdir;
// kept as a local literal so the retention package does not import the
// heavier investigator package just for one constant.
const investigationsSubdir = "investigations"

type Worker struct {
	store        *store.Store
	artifactRoot string
	keepDays     int
	scanEvery    time.Duration
	log          *slog.Logger
}

func New(st *store.Store, artifactRoot string, keepDays int, scanEvery time.Duration, log *slog.Logger) *Worker {
	if keepDays <= 0 {
		keepDays = 30
	}
	if scanEvery <= 0 {
		scanEvery = 1 * time.Hour
	}
	return &Worker{
		store: st, artifactRoot: artifactRoot,
		keepDays: keepDays, scanEvery: scanEvery, log: log,
	}
}

// Run blocks until ctx is cancelled, sweeping every scanEvery.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("retention worker started", "keep_days", w.keepDays, "scan_every", w.scanEvery)
	w.sweep(ctx) // immediate first sweep
	t := time.NewTicker(w.scanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.sweep(ctx)
		}
	}
}

func (w *Worker) sweep(ctx context.Context) {
	cutoff := time.Now().Add(-time.Duration(w.keepDays) * 24 * time.Hour)
	w.cleanupArtifacts(ctx, cutoff)
	w.cleanupInvestigationArtifacts(ctx, cutoff)
	w.cleanupArchivedMessages(ctx, cutoff)
}

// cleanupArtifacts walks artifactRoot, removes any task_id directory whose
// associated task finished before cutoff. Falls back to mtime when the task
// row is gone (covers manual / orphaned dirs).
func (w *Worker) cleanupArtifacts(ctx context.Context, cutoff time.Time) {
	entries, err := os.ReadDir(w.artifactRoot)
	if err != nil {
		w.log.Warn("retention: read artifact root", "err", err)
		return
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Investigation-scoped artifacts live under <root>/investigations and
		// have their own status-aware policy (cleanupInvestigationArtifacts).
		// Never treat that subtree as an orphan task directory.
		if e.Name() == investigationsSubdir {
			continue
		}
		taskID := e.Name()
		taskDir := filepath.Join(w.artifactRoot, taskID)
		// Try to find the task: if it exists and is not yet older than the
		// cutoff, leave it alone.
		t, err := w.store.GetTask(ctx, taskID)
		if err == nil && t.FinishedAt.Valid && t.FinishedAt.Time.After(cutoff) {
			continue
		}
		// Orphan or older — check mtime to be safe.
		info, err := e.Info()
		if err == nil && info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(taskDir); err != nil {
			w.log.Warn("retention: remove artifact dir", "dir", taskDir, "err", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		w.log.Info("retention: artifacts swept", "removed", removed, "cutoff", cutoff.Format(time.RFC3339))
	}
}

// cleanupInvestigationArtifacts applies a status-aware policy to the
// <root>/investigations/<investigation_id> subtree (notebooks, exports):
//   - live investigation (active/waiting/paused): always keep.
//   - terminal (done/aborted) within the retention window: keep.
//   - terminal and older than cutoff: remove.
//   - orphan (investigation row gone): fall back to directory mtime.
//
// It never deletes the artifact root or escapes it: directory names come
// straight from os.ReadDir (single path elements) and traversal-looking
// names are skipped defensively.
func (w *Worker) cleanupInvestigationArtifacts(ctx context.Context, cutoff time.Time) {
	base := filepath.Join(w.artifactRoot, investigationsSubdir)
	entries, err := os.ReadDir(base)
	if err != nil {
		if !os.IsNotExist(err) {
			w.log.Warn("retention: read investigations root", "err", err)
		}
		return
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		invID := e.Name()
		if invID == "." || invID == ".." || strings.ContainsAny(invID, `/\`) {
			continue
		}
		dir := filepath.Join(base, invID)
		inv, gerr := w.store.GetInvestigation(ctx, invID)
		status := "orphan"
		switch {
		case gerr != nil:
			// Orphan: keep while recent, else remove (covers manual dirs).
			if info, ierr := e.Info(); ierr == nil && info.ModTime().After(cutoff) {
				w.log.Debug("retention: investigation artifact kept",
					"investigation_id", invID, "status", status, "action", "keep")
				continue
			}
		case inv.Status == "active" || inv.Status == "waiting" || inv.Status == "paused":
			w.log.Debug("retention: investigation artifact kept",
				"investigation_id", invID, "status", inv.Status, "action", "keep")
			continue
		case inv.UpdatedAt.After(cutoff):
			w.log.Debug("retention: investigation artifact kept",
				"investigation_id", invID, "status", inv.Status, "action", "keep")
			continue
		default:
			status = inv.Status
		}
		if err := os.RemoveAll(dir); err != nil {
			w.log.Warn("retention: remove investigation artifact dir", "dir", dir, "err", err)
			continue
		}
		w.log.Debug("retention: investigation artifact removed",
			"investigation_id", invID, "status", status, "action", "remove")
		removed++
	}
	if removed > 0 {
		w.log.Info("retention: investigation artifacts swept",
			"removed", removed, "cutoff", cutoff.Format(time.RFC3339))
	}
}

// cleanupArchivedMessages drops messages where archived=1 AND the parent
// investigation finished_at is before cutoff. We keep archived messages
// while the investigation is live (operator may want to inspect them).
func (w *Worker) cleanupArchivedMessages(ctx context.Context, cutoff time.Time) {
	res, err := w.store.DB().ExecContext(ctx, `
        DELETE FROM messages
         WHERE archived = 1
           AND investigation_id IN (
              SELECT id FROM investigations
               WHERE status IN ('done','aborted')
                 AND COALESCE(updated_at, created_at) < ?
           )`, cutoff)
	if err != nil {
		w.log.Warn("retention: archived messages", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		w.log.Info("retention: archived messages purged", "rows", n)
	}
}
