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
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

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
