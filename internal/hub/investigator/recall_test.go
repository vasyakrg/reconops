package investigator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// T11: the compaction prompt MUST instruct the model to carry ids forward.
func TestCompactionPromptEchoesIDs(t *testing.T) {
	for _, want := range []string{"finding_id", "memory_id", "task_id", "VERBATIM"} {
		if !strings.Contains(compactionPrompt, want) {
			t.Errorf("compaction prompt missing id-echo directive %q", want)
		}
	}
}

// T11: the deterministic recall digest lists active findings with their
// finding_id + evidence task_ids, highest-severity first, dropping ignored.
func TestBuildFindingsDigest(t *testing.T) {
	if buildFindingsDigest(nil) != "" {
		t.Fatal("no findings → empty digest")
	}
	fs := []store.Finding{
		{ID: "f-warn", Severity: "warn", Code: "net.flap", Message: "carrier flapped", EvidenceJSON: `{"task_ids":["t3"]}`},
		{ID: "f-err", Severity: "error", Code: "disk.full", Message: "root fs at 100%", EvidenceJSON: `{"task_ids":["t7","t9"]}`},
		{ID: "f-ign", Severity: "critical", Code: "noise", Message: "ignored thing", Ignored: true, EvidenceJSON: `{"task_ids":["t1"]}`},
	}
	d := buildFindingsDigest(fs)
	for _, want := range []string{"finding_id=f-err", "disk.full", "t7, t9", "finding_id=f-warn", "t3"} {
		if !strings.Contains(d, want) {
			t.Fatalf("digest missing %q in:\n%s", want, d)
		}
	}
	if strings.Contains(d, "f-ign") || strings.Contains(d, "ignored thing") {
		t.Fatalf("ignored findings must be dropped:\n%s", d)
	}
	// error sorts before warn.
	if strings.Index(d, "f-err") > strings.Index(d, "f-warn") {
		t.Fatalf("higher severity must come first:\n%s", d)
	}
}

// T11 end-to-end: after compaction the findings digest survives as a system_note
// (seq past the archive cut), so the post-compaction model still has the ids.
func TestCompactInjectsFindingsDigest(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const inv = "inv-recall"
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: inv, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFinding(ctx, store.Finding{ID: "f-1", InvestigationID: inv, Severity: "error", Code: "disk.full", Message: "root fs 100%", EvidenceJSON: `{"task_ids":["task-7","task-9"]}`}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: inv, Role: "system", Content: "system"})
	_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: inv, Role: "user", Content: "goal"})
	for i := 0; i < compactionKeepRecent+4; i++ {
		_, _ = st.AppendMessage(ctx, store.Message{InvestigationID: inv, Role: "assistant", Content: "evidence"})
	}
	fakeLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"summary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`))
	}))
	t.Cleanup(fakeLLM.Close)
	client, err := llm.New(llm.Options{BaseURL: fakeLLM.URL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	l := NewLoop(st, client, nil, nil, nil, 10, 100000, nil)
	if err := l.compact(ctx, inv); err != nil {
		t.Fatal(err)
	}

	// The digest must be a LIVE (non-archived) system_note carrying the ids.
	msgs, err := st.ListMessages(ctx, inv, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "system_note" && strings.Contains(m.Content, "FINDINGS SO FAR") {
			found = true
			for _, want := range []string{"finding_id=f-1", "task-7", "task-9", "disk.full"} {
				if !strings.Contains(m.Content, want) {
					t.Fatalf("digest system_note missing %q: %s", want, m.Content)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no live findings-digest system_note after compaction; messages=%d", len(msgs))
	}
}
