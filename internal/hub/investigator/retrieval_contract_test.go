package investigator

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/logtriage"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// seedTaskWithArtifacts seeds an investigation + task whose result points at an
// on-disk artifact dir populated with the given files. Returns the dir.
func seedTaskWithArtifacts(t *testing.T, st *store.Store, invID, taskID string, dataJSON []byte, files map[string][]byte) string {
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
	if err := st.InsertTask(ctx, store.Task{ID: taskID, RunID: "run-" + taskID, HostID: "h1", Collector: "file_read", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertResult(ctx, store.Result{TaskID: taskID, DataJSON: dataJSON, ArtifactDir: dir}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The force escape hatch must be a declared schema property, else strict
// backends reject the steered retry; artifact_name must NOT be required.
func TestRetrievalSchemaContract(t *testing.T) {
	var sa, gfr map[string]any
	for _, tl := range Tools() {
		switch tl.Function.Name {
		case "search_artifact":
			if err := json.Unmarshal(tl.Function.Parameters, &sa); err != nil {
				t.Fatal(err)
			}
		case "get_full_result":
			if err := json.Unmarshal(tl.Function.Parameters, &gfr); err != nil {
				t.Fatal(err)
			}
		}
	}
	if sa == nil || gfr == nil {
		t.Fatal("missing search_artifact / get_full_result schema")
	}
	for _, r := range sa["required"].([]any) {
		if r == "artifact_name" {
			t.Error("artifact_name must be optional (not in required)")
		}
	}
	props := gfr["properties"].(map[string]any)
	if _, ok := props["force"]; !ok {
		t.Error("get_full_result schema must declare a force property")
	}
}

// The over-size get_full_result block must name the task's real artifacts so
// the model never has to guess one.
func TestPreflightRetrieval_OversizedBlockNamesArtifacts(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	big := []byte(`{"x":"` + strings.Repeat("a", getFullResultCap+10) + `"}`)
	seedTaskWithArtifacts(t, st, "inv-names", "task-big", big,
		map[string][]byte{"file_read_syslog.txt": []byte("hello\n")})
	l := &Loop{store: st}
	tc := &store.ToolCallRow{ID: "tc1", InvestigationID: "inv-names", Tool: "get_full_result", InputJSON: `{"task_id":"task-big"}`}
	res, blocked := l.preflightRetrieval(ctx, "inv-names", tc)
	if !blocked {
		t.Fatal("oversized result should be blocked")
	}
	if !strings.Contains(res.Error, "file_read_syslog.txt") {
		t.Fatalf("block message must name the real artifact, got: %s", res.Error)
	}
}

// An omitted artifact_name and the explicitly-typed default must dedup
// identically, or the search repeat-cap is trivially evaded.
func TestPreflightRetrieval_SearchRepeatCap_OmittedEqualsExplicit(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedTaskWithArtifacts(t, st, "inv-norm", "t1", []byte(`{}`),
		map[string][]byte{"file_read_syslog.txt": []byte("oom here\n")})
	explicit := `{"task_id":"t1","artifact_name":"file_read_syslog.txt","pattern":"oom"}`
	for i, id := range []string{"s1", "s2"} {
		if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: id, InvestigationID: "inv-norm", Seq: i + 1, Tool: "search_artifact", InputJSON: explicit, Status: "approved"}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateToolCall(ctx, id, "executed", "auto", "", "{}"); err != nil {
			t.Fatal(err)
		}
	}
	l := &Loop{store: st}
	// Third call OMITS artifact_name — must still be blocked (normalizes to the
	// single artifact, matching the explicit prior calls).
	tc := &store.ToolCallRow{ID: "s3", InvestigationID: "inv-norm", Tool: "search_artifact", InputJSON: `{"task_id":"t1","pattern":"oom"}`}
	if _, blocked := l.preflightRetrieval(ctx, "inv-norm", tc); !blocked {
		t.Fatal("omitted artifact_name must dedup against the explicit default and be blocked")
	}
}

func TestHandleSearchArtifact_ResolvesOptionalName(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedTaskWithArtifacts(t, st, "inv-r1", "t1", []byte(`{}`),
		map[string][]byte{"file_read_syslog.txt": []byte("alpha\nbravo error\ncharlie\n")})
	env := HandlerEnv{Store: st, InvestigationID: "inv-r1", MaxResultTokens: 500}
	res := handleSearchArtifact(ctx, env, `{"task_id":"t1","pattern":"error"}`)
	if !res.OK {
		t.Fatalf("omitted name on a single-artifact task should resolve: %s", res.Error)
	}
}

func TestHandleSearchArtifact_MultiArtifactOmittedListsNames(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedTaskWithArtifacts(t, st, "inv-r2", "t1", []byte(`{}`), map[string][]byte{
		"a.txt":                 []byte("x\n"),
		"b.txt":                 []byte("x\n"),
		logtriage.IndexFileName: []byte(`{"artifacts":[]}`),
	})
	env := HandlerEnv{Store: st, InvestigationID: "inv-r2", MaxResultTokens: 500}
	res := handleSearchArtifact(ctx, env, `{"task_id":"t1","pattern":"x"}`)
	if res.OK {
		t.Fatal("omitted name on a multi-artifact task must NOT auto-pick — expected an error")
	}
	// Must list the two real artifacts and exclude the _index.json sidecar.
	if !strings.Contains(res.Error, "a.txt") || !strings.Contains(res.Error, "b.txt") {
		t.Fatalf("error must enumerate valid names: %s", res.Error)
	}
	if strings.Contains(res.Error, logtriage.IndexFileName) || strings.Contains(res.Error, "3 artifacts") {
		t.Fatalf("index sidecar must be excluded from the artifact list: %s", res.Error)
	}
}

func TestHandleSearchArtifact_WrongNameListsValid(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedTaskWithArtifacts(t, st, "inv-r3", "t1", []byte(`{}`),
		map[string][]byte{"file_read_syslog.txt": []byte("x\n")})
	env := HandlerEnv{Store: st, InvestigationID: "inv-r3", MaxResultTokens: 500}
	res := handleSearchArtifact(ctx, env, `{"task_id":"t1","artifact_name":"guessed.txt","pattern":"x"}`)
	if res.OK {
		t.Fatal("a wrong artifact_name must error")
	}
	if !strings.Contains(res.Error, "file_read_syslog.txt") {
		t.Fatalf("wrong-name error must list the real artifact: %s", res.Error)
	}
}

func TestHandleSearchArtifact_BinaryRefused(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	seedTaskWithArtifacts(t, st, "inv-r4", "t1", []byte(`{}`),
		map[string][]byte{"dump.bin": {'a', 0x00, 'b', 0x00, 'c'}})
	env := HandlerEnv{Store: st, InvestigationID: "inv-r4", MaxResultTokens: 500}
	res := handleSearchArtifact(ctx, env, `{"task_id":"t1","pattern":"a"}`)
	if res.OK {
		t.Fatal("binary artifact must be refused")
	}
	if !strings.Contains(res.Error, "binary") {
		t.Fatalf("expected a binary-content error, got: %s", res.Error)
	}
}
