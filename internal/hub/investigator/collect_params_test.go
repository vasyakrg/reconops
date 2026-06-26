package investigator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// T8: the model sends JSON scalars for collect params even though the schema
// declares strings; paramMap coerces them at unmarshal instead of failing the
// whole tool call with "cannot unmarshal bool into string".
func TestParamMapCoercesScalars(t *testing.T) {
	var a collectArgs
	in := `{"host_id":"h","collector":"journal_tail","params":{` +
		`"kernel":true,"previous_boot":false,"tail_lines":200,` +
		`"max_bytes":1048576,"top_n":5,"offset":0,"path":"/var/log/x"}}`
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		t.Fatalf("unmarshal must not fail on scalar params: %v", err)
	}
	want := map[string]string{
		"kernel": "true", "previous_boot": "false", "tail_lines": "200",
		"max_bytes": "1048576", "top_n": "5", "offset": "0", "path": "/var/log/x",
	}
	for k, v := range want {
		if a.Params[k] != v {
			t.Errorf("param %q = %q, want %q", k, a.Params[k], v)
		}
	}
	// The float64 trap: a large int routed through fmt.Sprint(float64) becomes
	// "1.048576e+06" and breaks the collector. UseNumber must preserve digits.
	if a.Params["max_bytes"] != "1048576" {
		t.Fatalf("max_bytes coerced to %q — scientific-notation regression", a.Params["max_bytes"])
	}
}

func TestParamMapNullAndBatch(t *testing.T) {
	var a collectArgs
	if err := json.Unmarshal([]byte(`{"host_id":"h","collector":"c"}`), &a); err != nil {
		t.Fatalf("missing params must be fine: %v", err)
	}
	if len(a.Params) != 0 {
		t.Fatalf("absent params should be empty, got %v", a.Params)
	}
	var b collectBatchArgs
	if err := json.Unmarshal([]byte(`{"host_ids":["h1","h2"],"collector":"c","params":{"lines":50,"from_end":true}}`), &b); err != nil {
		t.Fatalf("batch scalar params: %v", err)
	}
	if b.Params["lines"] != "50" || b.Params["from_end"] != "true" {
		t.Fatalf("batch params not coerced: %v", b.Params)
	}
}

func TestSearchArtifactRE2LookaroundError(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: "inv-re", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRun(ctx, store.Run{ID: "run-re", Name: "c", CreatedBy: "o", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHost(ctx, store.Host{ID: "h1", Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "fp", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, store.Task{ID: "task-re", RunID: "run-re", HostID: "h1", Collector: "journal", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	artDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(artDir, "j.log"), []byte("kernel panic at boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertResult(ctx, store.Result{TaskID: "task-re", DataJSON: []byte(`{}`), ArtifactDir: artDir}); err != nil {
		t.Fatal(err)
	}

	env := HandlerEnv{Store: st, InvestigationID: "inv-re", MaxResultTokens: 500}
	res := handleSearchArtifact(ctx, env, `{"task_id":"task-re","artifact_name":"j.log","pattern":"kernel(?=panic)"}`)
	if res.OK {
		t.Fatalf("a lookahead pattern must be rejected by RE2: %+v", res)
	}
	if !strings.Contains(res.Error, "lookahead") || !strings.Contains(res.Error, "RE2") {
		t.Fatalf("error must name the RE2 lookaround limitation: %q", res.Error)
	}

	// A normal (RE2-valid) pattern still works.
	ok := handleSearchArtifact(ctx, env, `{"task_id":"task-re","artifact_name":"j.log","pattern":"panic"}`)
	if !ok.OK {
		t.Fatalf("a valid pattern must still succeed: %+v", ok)
	}
}
