package investigator

import (
	"context"
	"testing"
	"time"
)

// buildTaskViews must surface the collect's finish time so the model can derive
// incident/boot time from a relative field (system_info.uptime_sec) instead of
// drifting off wall-clock "now" over a long investigation.
func TestBuildTaskViews_CollectedAt(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedTaskWithResult(t, st, "inv-ts", "task-ts", []byte(`{"uptime_sec":857004}`))
	if err := st.FinishTask(ctx, "task-ts", "done", 12, ""); err != nil {
		t.Fatal(err)
	}
	views := buildTaskViews(ctx, HandlerEnv{Store: st, InvestigationID: "inv-ts"}, []string{"task-ts"})
	if len(views) != 1 {
		t.Fatalf("want 1 view, got %d", len(views))
	}
	if views[0].CollectedAt == "" {
		t.Fatal("CollectedAt must be set from task.FinishedAt")
	}
	if _, err := time.Parse(time.RFC3339, views[0].CollectedAt); err != nil {
		t.Fatalf("CollectedAt is not RFC3339: %q (%v)", views[0].CollectedAt, err)
	}
}
