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

	"github.com/vasyakrg/recon/internal/hub/store"
)

func seedTaskWithResult(t *testing.T, st *store.Store, invID, taskID string, dataJSON []byte) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: invID, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRun(ctx, store.Run{ID: "run-" + taskID, InvestigationID: sql.NullString{String: invID, Valid: true}, Name: "c", CreatedBy: "o", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHost(ctx, store.Host{ID: "h1", Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "fp", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, store.Task{ID: taskID, RunID: "run-" + taskID, HostID: "h1", Collector: "system_info", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertResult(ctx, store.Result{TaskID: taskID, DataJSON: dataJSON}); err != nil {
		t.Fatal(err)
	}
}

// seedOversizedWithArtifact seeds an oversized result that DOES have a
// searchable artifact, so the oversize guard can legitimately steer to
// search_artifact.
func seedOversizedWithArtifact(t *testing.T, st *store.Store, invID, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: invID, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRun(ctx, store.Run{ID: "run-" + taskID, InvestigationID: sql.NullString{String: invID, Valid: true}, Name: "c", CreatedBy: "o", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHost(ctx, store.Host{ID: "h1", Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "fp", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, store.Task{ID: taskID, RunID: "run-" + taskID, HostID: "h1", Collector: "journal", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	artDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(artDir, "journal.log"), []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := []byte(`{"x":"` + strings.Repeat("a", getFullResultCap+10) + `"}`)
	if err := st.UpsertResult(ctx, store.Result{TaskID: taskID, DataJSON: big, ArtifactDir: artDir}); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightRetrieval_OversizedFullResult(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Oversized result WITH a searchable artifact: the guard still steers to
	// search_artifact (artifact-less is the only case that now proceeds — T7).
	seedOversizedWithArtifact(t, st, "inv-gfr", "task-big")
	l := &Loop{store: st}

	tc := &store.ToolCallRow{ID: "tc1", InvestigationID: "inv-gfr", Tool: "get_full_result", InputJSON: `{"task_id":"task-big"}`}
	if _, blocked := l.preflightRetrieval(ctx, "inv-gfr", tc); !blocked {
		t.Fatal("oversized get_full_result WITH an artifact should be blocked")
	}
	tcForce := &store.ToolCallRow{ID: "tc2", InvestigationID: "inv-gfr", Tool: "get_full_result", InputJSON: `{"task_id":"task-big","force":true}`}
	if _, blocked := l.preflightRetrieval(ctx, "inv-gfr", tcForce); blocked {
		t.Fatal("force:true should bypass the oversized guard")
	}
}

// T7: an oversized STRUCTURED result with NO searchable artifact must NOT be
// blocked toward search_artifact (which dead-ends) — it proceeds to
// handleGetFullResult, which returns a bounded, pageable window.
func TestPreflightRetrieval_OversizedArtifactlessProceeds(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	big := []byte(`{"x":"` + strings.Repeat("a", getFullResultCap+10) + `"}`)
	seedTaskWithResult(t, st, "inv-al", "task-al", big) // no ArtifactDir
	l := &Loop{store: st}
	tc := &store.ToolCallRow{ID: "tc1", InvestigationID: "inv-al", Tool: "get_full_result", InputJSON: `{"task_id":"task-al"}`}
	if _, blocked := l.preflightRetrieval(ctx, "inv-al", tc); blocked {
		t.Fatal("artifact-less oversized get_full_result must proceed, not dead-end at search_artifact")
	}
}

func TestHandleGetFullResult_WindowsOversized(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	big := []byte(`{"x":"` + strings.Repeat("a", getFullResultCap*2) + `"}`)
	total := len(big)
	seedTaskWithResult(t, st, "inv-w", "task-w", big)
	env := HandlerEnv{Store: st, InvestigationID: "inv-w"}

	// First window: bounded, not the whole body, with a next_offset.
	res := handleGetFullResult(ctx, env, `{"task_id":"task-w"}`)
	if !res.OK {
		t.Fatalf("get_full_result should succeed: %+v", res)
	}
	d := res.Data.(map[string]any)
	if d["truncated"] != true {
		t.Fatalf("oversized result must be marked truncated: %+v", d)
	}
	if d["data_window"] == nil || len(d["data_window"].(string)) > getFullResultCap {
		t.Fatalf("window must be present and within the cap: %d", len(d["data_window"].(string)))
	}
	if d["total_bytes"].(int) != total {
		t.Fatalf("total_bytes = %v, want %d", d["total_bytes"], total)
	}
	next, ok := d["next_offset"].(int)
	if !ok || next != getFullResultCap {
		t.Fatalf("next_offset = %v, want %d", d["next_offset"], getFullResultCap)
	}

	// Paging with offset continues the window.
	res2 := handleGetFullResult(ctx, env, fmt.Sprintf(`{"task_id":"task-w","offset":%d}`, next))
	d2 := res2.Data.(map[string]any)
	if d2["offset"].(int) != next {
		t.Fatalf("offset not honored: %+v", d2)
	}

	// A small result still returns the full parsed structured data.
	seedTaskWithResult(t, st, "inv-sm", "task-sm", []byte(`{"k":42}`))
	envSm := HandlerEnv{Store: st, InvestigationID: "inv-sm"}
	resSm := handleGetFullResult(ctx, envSm, `{"task_id":"task-sm"}`)
	dSm := resSm.Data.(map[string]any)
	if dSm["truncated"] == true {
		t.Fatalf("small result must not be truncated: %+v", dSm)
	}
	if dSm["data"] == nil {
		t.Fatalf("small result must return parsed data: %+v", dSm)
	}
}

func TestPreflightRetrieval_SmallFullResultPasses(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedTaskWithResult(t, st, "inv-small", "task-small", []byte(`{"x":1}`))
	l := &Loop{store: st}
	tc := &store.ToolCallRow{ID: "tc1", InvestigationID: "inv-small", Tool: "get_full_result", InputJSON: `{"task_id":"task-small"}`}
	if _, blocked := l.preflightRetrieval(ctx, "inv-small", tc); blocked {
		t.Fatal("small get_full_result should pass")
	}
}

func TestCapSearchMatchesDropsContextThenMatches(t *testing.T) {
	mk := func(n int, withContext bool) []searchMatch {
		out := make([]searchMatch, 0, n)
		for i := 0; i < n; i++ {
			m := searchMatch{LineNo: i + 1, Text: strings.Repeat("oom-killer invoked ", 8)}
			if withContext {
				m.Context = []string{strings.Repeat("ctx ", 40), strings.Repeat("ctx ", 40)}
			}
			out = append(out, m)
		}
		return out
	}
	fixed := map[string]any{"task_id": "t1", "artifact": "j.log", "truncated": false}

	// Context-heavy set: dropping context alone should bring it under budget.
	kept, omitted, droppedContext, steps := capSearchMatches(mk(8, true), fixed, 300)
	if !droppedContext {
		t.Fatalf("expected context to be dropped first, steps=%v", steps)
	}
	for _, m := range kept {
		if len(m.Context) != 0 {
			t.Fatalf("context must be dropped when over budget: %+v", m)
		}
		if m.LineNo == 0 {
			t.Fatalf("line refs must be preserved on kept matches")
		}
	}
	_ = omitted

	// Many matches even without context: tail matches must be omitted.
	kept2, omitted2, _, steps2 := capSearchMatches(mk(200, false), fixed, 200)
	if omitted2 == 0 {
		t.Fatalf("expected matches omitted under a tight budget, steps=%v", steps2)
	}
	if len(kept2) < 1 {
		t.Fatalf("at least one match must remain as an anchor")
	}
	if len(kept2)+omitted2 != 200 {
		t.Fatalf("kept+omitted must account for all matches: %d+%d", len(kept2), omitted2)
	}
}

func TestHandleSearchArtifactCapsOutput(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	artDir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "2026-06-20T10:00:%02dZ host kernel: oom-killer invoked for process %d killed\n", i%60, i)
	}
	if err := os.WriteFile(filepath.Join(artDir, "journal.log"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := st.InsertInvestigation(ctx, store.Investigation{ID: "inv-cap", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRun(ctx, store.Run{ID: "run-cap", InvestigationID: sql.NullString{String: "inv-cap", Valid: true}, Name: "c", CreatedBy: "o", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHost(ctx, store.Host{ID: "h1", Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "fp", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, store.Task{ID: "task-cap", RunID: "run-cap", HostID: "h1", Collector: "journal", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertResult(ctx, store.Result{TaskID: "task-cap", DataJSON: []byte(`{}`), ArtifactDir: artDir}); err != nil {
		t.Fatal(err)
	}

	env := HandlerEnv{Store: st, InvestigationID: "inv-cap", MaxResultTokens: 500}
	res := handleSearchArtifact(ctx, env, `{"task_id":"task-cap","artifact_name":"journal.log","pattern":"oom-killer","max_matches":500,"context_lines":5}`)
	if !res.OK {
		t.Fatalf("search should succeed: %+v", res)
	}
	data := res.Data.(map[string]any)
	omitted, _ := data["omitted_matches"].(int)
	if omitted <= 0 {
		t.Fatalf("a wide search over a 500-token budget must omit matches: %+v", data)
	}
	if data["_hint"] == nil {
		t.Fatalf("capped search must carry a steering _hint")
	}
	matches := data["matches"].([]searchMatch)
	if len(matches) == 0 {
		t.Fatalf("at least one match must be returned")
	}
	for _, m := range matches {
		if m.LineNo == 0 {
			t.Fatalf("returned matches must keep line refs: %+v", m)
		}
	}
	// Confirm the assembled result is actually within budget.
	body, _ := json.Marshal(res)
	if tok := tokensForBytes(len(body)); tok > 700 {
		t.Fatalf("capped result still too large: %d tokens", tok)
	}
}

func TestPreflightRetrieval_SearchRepeatCap(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: "inv-sa", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	input := `{"task_id":"t1","artifact_name":"j.log","pattern":"oom"}`
	for i, id := range []string{"s1", "s2"} {
		if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: id, InvestigationID: "inv-sa", Seq: i + 1, Tool: "search_artifact", InputJSON: input, Status: "approved"}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateToolCall(ctx, id, "executed", "auto", "", "{}"); err != nil {
			t.Fatal(err)
		}
	}
	l := &Loop{store: st}
	tc := &store.ToolCallRow{ID: "s3", InvestigationID: "inv-sa", Tool: "search_artifact", InputJSON: input}
	if _, blocked := l.preflightRetrieval(ctx, "inv-sa", tc); !blocked {
		t.Fatal("third identical search_artifact should be blocked")
	}
	tcDiff := &store.ToolCallRow{ID: "s4", InvestigationID: "inv-sa", Tool: "search_artifact", InputJSON: `{"task_id":"t1","artifact_name":"j.log","pattern":"panic"}`}
	if _, blocked := l.preflightRetrieval(ctx, "inv-sa", tcDiff); blocked {
		t.Fatal("different pattern should pass")
	}
}
