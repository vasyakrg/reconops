package investigator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/agent/collect"
	// Blank import registers the real file_read collector so this test drives
	// the genuine collector → persist → SummarizeTasks path, not a hand-crafted
	// stand-in. If file_read's envelope changes, this test catches it.
	_ "github.com/vasyakrg/recon/internal/agent/collectors/files"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// TestFileReadTokenEconomyEndToEnd proves the Task 1 economy property end to
// end: a ~1.2 MiB file_read keeps the body OUT of the context (it lives only in
// the artifact), so DataJSON stays under the get_full_result cap, the collect
// result the model sees stays within the per-result budget, and the result is
// still demotable to a task_id re-read pointer.
func TestFileReadTokenEconomyEndToEnd(t *testing.T) {
	ctx := context.Background()

	var b strings.Builder
	for i := 0; b.Len() < 1200*1024; i++ {
		fmt.Fprintf(&b, "2026-06-20T10:%02d:%02dZ host kernel: tpm tpm0: timeout waiting for cmd %d\n", (i/60)%60, i%60, i)
	}
	path := "/var/log/recon_test_te.log"
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Skipf("cannot write probe file at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	c, ok := collect.Get("file_read")
	if !ok {
		t.Fatal("file_read collector not registered")
	}
	res, err := c.Run(ctx, collect.Params{"path": path, "max_bytes": "1048576"})
	if err != nil {
		t.Fatalf("file_read: %v", err)
	}

	dataJSON, _ := json.Marshal(res.Data)
	// (b) The body left DataJSON entirely — metadata is far under the cap, so
	// get_full_result is never blocked and never re-inflates context with a body.
	if len(dataJSON) >= getFullResultCap {
		t.Fatalf("file_read DataJSON must be << getFullResultCap (%d), got %d", getFullResultCap, len(dataJSON))
	}
	if len(res.Artifacts) != 1 || len(res.Artifacts[0].Body) < 900*1024 {
		t.Fatalf("the ~1 MiB body must live in the artifact, got %d artifacts / body len %d",
			len(res.Artifacts), artBodyLen(res.Artifacts))
	}

	// Persist exactly as the hub runner would: artifact on disk + metadata DataJSON.
	st := openTestStore(t)
	const inv, run, task = "inv-te", "run-te", "task-te"
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: inv, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRun(ctx, store.Run{ID: run, InvestigationID: sql.NullString{String: inv, Valid: true}, Name: "c", CreatedBy: "o", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHost(ctx, store.Host{ID: "h1", Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "fp", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, store.Task{ID: task, RunID: run, HostID: "h1", Collector: "file_read", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	art := res.Artifacts[0]
	if err := os.WriteFile(filepath.Join(dir, art.Name), art.Body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertResult(ctx, store.Result{TaskID: task, DataJSON: dataJSON, ArtifactDir: dir}); err != nil {
		t.Fatal(err)
	}

	// (b') The STORED DataJSON is under the cap, so the preflight never blocks
	// get_full_result for this task.
	gr, err := st.GetResult(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(gr.DataJSON) >= getFullResultCap {
		t.Fatalf("stored DataJSON over get_full_result cap: %d", len(gr.DataJSON))
	}

	// (a) The collect result the model sees stays within the per-result budget,
	// even though the underlying file is ~1.2 MiB.
	const budget = 2000
	env := HandlerEnv{Store: st, InvestigationID: inv, MaxResultTokens: budget}
	sres := SummarizeTasks(ctx, env, []string{task})
	if !sres.OK {
		t.Fatalf("summarize failed: %+v", sres)
	}
	envJSON, _ := json.Marshal(sres)
	if tok := tokensForBytes(len(envJSON)); tok > budget {
		t.Fatalf("file_read result exceeded budget: %d tokens (cap %d)", tok, budget)
	}

	// (c) The collect envelope stays demotable to a task_id pointer — Task 4 must
	// not change the {data:{tasks:[{task_id}]}} shape, or an aged result loses
	// its re-read path.
	ptr, ok := demotionPointer("collect", string(envJSON))
	if !ok || !strings.Contains(ptr, task) || !strings.Contains(ptr, "get_full_result") {
		t.Fatalf("file_read result must demote to a re-read pointer naming the task_id: ok=%v ptr=%q", ok, ptr)
	}
}

func artBodyLen(arts []collect.Artifact) int {
	if len(arts) == 0 {
		return 0
	}
	return len(arts[0].Body)
}
