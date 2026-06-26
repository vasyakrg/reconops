package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// T4: under an armed auto-run a proposed mark_done is held for the operator (by
// design). It must render as a Conclusion-review card that explains the hold and
// surfaces the proposed root cause + confidence — not the generic raw-JSON
// "Pending approval" card that read as a stuck probe.

func armedPendingMarkDone(t *testing.T, st *store.Store, id string, armed bool) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: id, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if armed {
		if err := st.SetAutonomousRun(ctx, id, 35, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-" + id, InvestigationID: id, Seq: 12, Tool: "mark_done",
		InputJSON: `{"summary":{"root_cause":"disk full on /var starved nginx","confidence":"likely","recommended_remediation":"clear logs","symptoms":["upstream 502"]}}`,
		Status:    "pending",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAutonomousTerminalHold_RendersConclusionCard(t *testing.T) {
	srv, st := newTestServer(t)
	armedPendingMarkDone(t, st, "inv-held", true)
	sid, _, err := srv.sessions.issue(context.Background(), "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := fetchFragment(t, srv, sid, "inv-held")
	for _, want := range []string{
		"Conclusion — approve to close",             // card header, not "Pending approval"
		"Auto-run paused here for your review",      // explains the hold
		"disk full on /var starved nginx",           // proposed root cause surfaced
		`class="badge info"`,                        // confidence "likely" -> info badge
		">likely<",                                  // confidence label
		"Approve &amp; close",                       // close-labelled action
		"auto-run · paused — review the conclusion", // header badge reflects the hold
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("held mark_done card missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "◇ Pending approval") {
		t.Fatalf("a held mark_done must use the conclusion card, not the generic approval card")
	}
}

func TestManualPendingMarkDone_UsesConclusionCardWithoutAutoCopy(t *testing.T) {
	srv, st := newTestServer(t)
	armedPendingMarkDone(t, st, "inv-manual-md", false)
	sid, _, err := srv.sessions.issue(context.Background(), "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := fetchFragment(t, srv, sid, "inv-manual-md")
	if !strings.Contains(body, "Conclusion — approve to close") {
		t.Fatalf("manual pending mark_done should still use the conclusion card:\n%s", body)
	}
	if strings.Contains(body, "Auto-run paused here for your review") {
		t.Fatalf("non-armed pending mark_done must not show the auto-run hold copy")
	}
	if strings.Contains(body, "◇ Pending approval") {
		t.Fatalf("mark_done must not fall through to the generic approval card")
	}
}

// The model emits Markdown in its mark_done rationale (headings, fenced config
// dumps, bold). The conclusion card must render it as BLOCK Markdown — flatten
// it inline (newlines → spaces) and a multi-section report collapses into one
// unreadable run-on line, which is the "where is the promised markdown?" bug.
func TestPendingMarkDoneRationale_RendersBlockMarkdown(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-md-rat", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	rationale := "I have the full DNS config for `host-x`:\n\n" +
		"**/etc/resolv.conf**\n\n" +
		"```\nsearch internal.qbix.ru\nnameserver 192.168.200.23\n```\n\n" +
		"Closing now."
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-inv-md-rat", InvestigationID: "inv-md-rat", Seq: 16, Tool: "mark_done",
		InputJSON: `{"summary":{"root_cause":"not an incident","confidence":"confirmed"}}`,
		Rationale: rationale,
		Status:    "pending",
	}); err != nil {
		t.Fatal(err)
	}

	body := fragmentBody(t, srv, "inv-md-rat")

	// Fenced code block must survive as a <pre> block, and the bold header as
	// <strong> — neither is possible once newlines are collapsed inline.
	for _, want := range []string{
		"<pre><code>",
		"nameserver 192.168.200.23",
		"<strong>/etc/resolv.conf</strong>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("mark_done rationale not block-rendered, missing %q in:\n%s", want, body)
		}
	}
	// And the literal fence must NOT leak as text (the inline-flattened symptom).
	if strings.Contains(body, "```") {
		t.Fatalf("raw ``` fence leaked into rendered rationale (still inline?):\n%s", body)
	}
}
