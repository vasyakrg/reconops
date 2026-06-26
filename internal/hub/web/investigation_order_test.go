package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// T2 evidence anchors: a collect_batch tool_call stores a comma-joined TaskID
// ("t1,t2,t3"), but a mark_done evidence_ref is always a single task_id. The
// timeline must emit one anchor per id so #ev-t2 resolves, not a dead
// id="ev-t1,t2,t3".
func TestTimeline_PerTaskEvidenceAnchors(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-anchor", Goal: "g", Status: "active", CreatedBy: "op", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-batch", InvestigationID: "inv-anchor", Seq: 1, Tool: "collect_batch",
		InputJSON: `{}`, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	// task_id is persisted by UpdateToolCall (the decided path), not InsertToolCall.
	// A collect_batch stores the per-host ids comma-joined.
	if err := st.UpdateToolCall(ctx, "tc-batch", "executed", "auto", "t1,t2,t3", "{}"); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/investigations/fragments/inv-anchor", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	body := rw.Body.String()
	for _, id := range []string{`id="ev-t1"`, `id="ev-t2"`, `id="ev-t3"`} {
		if !strings.Contains(body, id) {
			t.Fatalf("per-task anchor %s missing — collect_batch evidence link is dead:\n%s", id, body)
		}
	}
	if strings.Contains(body, `id="ev-t1,t2,t3"`) {
		t.Fatalf("joined-id anchor must not be emitted (it never matches a single evidence_ref)")
	}
}

// T1: the operator-facing detail page must show newest activity first, while
// the store lists stay ascending for the tail-first investigator gate walks.

func TestTimelineView_NewestFirst(t *testing.T) {
	in := []store.ToolCallRow{
		{ID: "a", Seq: 1}, {ID: "b", Seq: 2}, {ID: "c", Seq: 3},
	}
	got := timelineView(in)
	wantSeq := []int{3, 2, 1}
	if len(got) != len(wantSeq) {
		t.Fatalf("len = %d, want %d", len(got), len(wantSeq))
	}
	for i, w := range wantSeq {
		if got[i].Seq != w {
			t.Fatalf("pos %d: seq %d, want %d (order=%v)", i, got[i].Seq, w, seqs(got))
		}
	}
	// Input must be untouched (web layer must not mutate the store slice).
	if in[0].Seq != 1 || in[2].Seq != 3 {
		t.Fatalf("input slice was mutated: %v", seqs(in))
	}
}

func TestFindingsView_NewestFirstWithinPinIgnoreGroup(t *testing.T) {
	base := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	// Deliberately interleaved across groups and times. Store order is
	// (pinned DESC, ignored ASC, created_at ASC); the view must keep the group
	// partition but flip created_at to newest-first inside each group.
	in := []store.Finding{
		{ID: "pin-old", Pinned: true, CreatedAt: at(1)},
		{ID: "pin-new", Pinned: true, CreatedAt: at(5)},
		{ID: "act-old", CreatedAt: at(2)},
		{ID: "act-new", CreatedAt: at(6)},
		{ID: "ign-old", Ignored: true, CreatedAt: at(3)},
		{ID: "ign-new", Ignored: true, CreatedAt: at(7)},
	}
	got := findingsView(in)
	want := []string{"pin-new", "pin-old", "act-new", "act-old", "ign-new", "ign-old"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Fatalf("pos %d: %q, want %q (order=%v)", i, got[i].ID, w, ids(got))
		}
	}
	// A pinned+ignored finding stays in the pinned group (pinned DESC dominates).
	if in[0].ID != "pin-old" {
		t.Fatalf("input slice was mutated: %v", ids(in))
	}
}

// TestInvestigationDetailData_OrdersNewestFirst pins the wiring: the data
// builder feeds the reversed views (not the raw ascending store lists) to the
// template.
func TestInvestigationDetailData_OrdersNewestFirst(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-order", Goal: "g", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	for seq := 1; seq <= 3; seq++ {
		if err := st.InsertToolCall(ctx, store.ToolCallRow{
			ID:              "tc-" + string(rune('0'+seq)),
			InvestigationID: "inv-order", Seq: seq, Tool: "collect",
			InputJSON: `{}`, Status: "executed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// active first, then pinned, then ignored — inserted out of group order so
	// the assertion catches a raw pass-through.
	for _, f := range []store.Finding{
		{ID: "f-act", InvestigationID: "inv-order", Severity: "warn", Code: "c.act"},
		{ID: "f-pin", InvestigationID: "inv-order", Severity: "warn", Code: "c.pin", Pinned: true},
		{ID: "f-ign", InvestigationID: "inv-order", Severity: "warn", Code: "c.ign", Ignored: true},
	} {
		if err := st.AddFinding(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	data, err := srv.investigationDetailData(ctx, "inv-order")
	if err != nil {
		t.Fatal(err)
	}
	tcs, ok := data["ToolCalls"].([]store.ToolCallRow)
	if !ok {
		t.Fatalf("ToolCalls type = %T", data["ToolCalls"])
	}
	if len(tcs) != 3 || tcs[0].Seq != 3 || tcs[2].Seq != 1 {
		t.Fatalf("timeline not newest-first: %v", seqs(tcs))
	}
	finds, ok := data["Findings"].([]store.Finding)
	if !ok {
		t.Fatalf("Findings type = %T", data["Findings"])
	}
	if len(finds) != 3 || finds[0].ID != "f-pin" || finds[1].ID != "f-act" || finds[2].ID != "f-ign" {
		t.Fatalf("findings group order wrong: %v", ids(finds))
	}
}

func seqs(tcs []store.ToolCallRow) []int {
	out := make([]int, len(tcs))
	for i, tc := range tcs {
		out[i] = tc.Seq
	}
	return out
}

func ids(fs []store.Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}
