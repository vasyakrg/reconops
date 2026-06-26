package investigator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// seedLogTask seeds one host + task + result whose artifact dir holds a
// clustered log file, so SummarizeTasks exercises the real index + roll-up +
// budget path.
func seedLogTask(t *testing.T, st *store.Store, invID, runID, taskID, hostID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertHost(ctx, store.Host{ID: hostID, Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "fp-" + hostID, Status: "online"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "2026-06-20T10:00:%02dZ %s kernel: oom-killer invoked, killed process %d\n", i%60, hostID, 1000+i)
	}
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "2026-06-20T10:01:%02dZ %s app[42]: ERROR connection refused to 10.0.0.%d:5432\n", i%60, hostID, i%250)
	}
	if err := os.WriteFile(filepath.Join(dir, "journal.log"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, store.Task{ID: taskID, RunID: runID, HostID: hostID, Collector: "journal", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertResult(ctx, store.Result{TaskID: taskID, DataJSON: []byte(`{"lines":600}`), ArtifactDir: dir}); err != nil {
		t.Fatal(err)
	}
}

// TestSummarizeTasksFleetBatchIsBounded is the headline "huge log arrays"
// integration check: a 30-host journal survey must return ONE bounded result
// (rolled up across hosts) within the per-result token budget, while keeping
// per-host drill-in keys (task_id + artifact name) so search_artifact works.
func TestSummarizeTasksFleetBatchIsBounded(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: "inv-fleet", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRun(ctx, store.Run{ID: "run-fleet", InvestigationID: sql.NullString{String: "inv-fleet", Valid: true}, Name: "survey", CreatedBy: "o", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	taskIDs := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("task-%02d", i)
		seedLogTask(t, st, "inv-fleet", "run-fleet", id, fmt.Sprintf("host-%02d", i))
		taskIDs = append(taskIDs, id)
	}

	const budget = 2000
	env := HandlerEnv{Store: st, InvestigationID: "inv-fleet", MaxResultTokens: budget}
	res := SummarizeTasks(ctx, env, taskIDs)
	if !res.OK {
		t.Fatalf("summarize should succeed: %+v", res)
	}

	// Whole result must be within budget — NOT the 25–50K tokens a naive
	// per-host index dump would be.
	body, _ := json.Marshal(res)
	if tok := tokensForBytes(len(body)); tok > budget {
		t.Fatalf("fleet batch result exceeded budget: %d tokens (cap %d)", tok, budget)
	}

	data := res.Data.(map[string]any)
	br, ok := data["batch_rollup"].(*batchRollup)
	if !ok {
		t.Fatalf("fleet batch must roll up across hosts, got %T", data["batch_rollup"])
	}
	if br.Collector != "journal" || len(br.Clusters) == 0 {
		t.Fatalf("rollup missing clusters: %+v", br)
	}
	// Per-host drill-in must still be possible.
	tasks := data["tasks"].([]taskView)
	if len(tasks) == 0 {
		t.Fatal("no per-host headlines retained for drill-in")
	}
	anchor := tasks[0]
	if anchor.TaskID == "" || len(anchor.Artifacts) == 0 {
		t.Fatalf("drill-in headline missing task_id/artifact: %+v", anchor)
	}
}

// captureLLM is a fake OpenAI-compatible endpoint that records the last request
// body and returns a fixed tool_call response with a cached-token count.
type captureLLM struct {
	mu       sync.Mutex
	lastBody string
}

func (c *captureLLM) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.lastBody = string(buf)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"x","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"need scope",
			  "tool_calls":[{"id":"call_1","type":"function","function":{"name":"ask_operator","arguments":"{\"question\":\"which host runs etcd?\"}"}}]},
			  "finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":6000,"completion_tokens":20,"total_tokens":6020,"prompt_tokens_details":{"cached_tokens":5000}}
		}`))
	}
}

// TestCallLLMCacheAccountingAndBreakpoint drives one real turn through a
// cache-capable router and asserts: (a) the wire request carried a cache_control
// breakpoint on the stable prefix, and (b) provider-reported cached tokens were
// accounted on the investigation, and (c) the token-estimate ratio calibrated.
func TestCallLLMCacheAccountingAndBreakpoint(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	fake := &captureLLM{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	t.Setenv("RECON_TEST_LLM_KEY", "k")
	router, err := llm.NewRouter([]llm.Profile{{
		Name: "primary", Role: "primary", Model: "m", BaseURL: srv.URL,
		APIKeyEnv: "RECON_TEST_LLM_KEY", SupportsTools: true, SupportsPromptCache: true,
		MaxOutputTokens: 4096, ContextWindowTokens: 200000,
	}})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.InsertInvestigation(ctx, store.Investigation{ID: "inv-cache", Goal: "diagnose", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-cache", Role: "system", Content: strings.Repeat("stable system prompt. ", 200)})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: "inv-cache", Role: "user", Content: "etcd is flapping"})

	l := &Loop{
		store:           st,
		maxSteps:        10,
		maxTokens:       1_000_000,
		running:         map[string]bool{},
		compactCooldown: map[string]time.Time{},
	}
	l.SetRouter(router)
	l.advance(ctx, "inv-cache")

	fake.mu.Lock()
	body := fake.lastBody
	fake.mu.Unlock()
	if !strings.Contains(body, "cache_control") || !strings.Contains(body, "ephemeral") {
		t.Fatalf("cache-capable route must send a cache_control breakpoint; body=%s", body)
	}

	inv, err := st.GetInvestigation(ctx, "inv-cache")
	if err != nil {
		t.Fatal(err)
	}
	if inv.TotalCachedTokens != 5000 {
		t.Fatalf("cached tokens not accounted: got %d want 5000", inv.TotalCachedTokens)
	}
	if inv.TokenCalibrationRatio <= 0 {
		t.Fatalf("calibration ratio should be set after a 6000-token turn: %v", inv.TokenCalibrationRatio)
	}
}
