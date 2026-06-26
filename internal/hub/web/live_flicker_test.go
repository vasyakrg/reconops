package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// TestNoFlickerWhileAwaitingApproval pins the residual "twitch" regression.
//
// While the operator sits on a pending-approval prompt and the LLM is idle,
// the investigation status stays "active" (a held tool_call does NOT flip the
// status to "waiting" — only ask_operator does), so the #live-pulse badge is
// rendered. Two independent things therefore must NOT churn:
//
//  1. SERVER: the live fragment HTML (and all three per-region hashes) must be
//     byte-identical across repeated polls when nothing changed, so the
//     client's hash gate skips every swap.
//  2. CLIENT: the always-on backstop poll must not rewrite the live-pulse
//     badge label on every tick — the badge has no reserved width, so a label
//     length change ("live" ↔ "updating") reflows the whole header flex row.
func TestNoFlickerWhileAwaitingApproval(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-probe", Goal: "diagnose", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	// A few executed tool calls + one pending (awaiting approval), <10 total —
	// the exact short-timeline shape in which the twitch was reported.
	for i := 1; i <= 4; i++ {
		if err := st.InsertToolCall(ctx, store.ToolCallRow{
			ID: "tc-done-" + string(rune('0'+i)), InvestigationID: "inv-probe", Seq: i,
			Tool: "collect", InputJSON: `{"host_id":"h1","collector":"c"}`,
			Rationale: "step", Status: "executed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-pending", InvestigationID: "inv-probe", Seq: 5,
		Tool: "collect", InputJSON: `{"host_id":"h1","collector":"c2"}`,
		Rationale: "needs approval", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// (1) Server stability: two idle renders must be byte-identical, and every
	// region hash must be stable, so the client's per-region gate never swaps.
	b1 := fetchFragment(t, srv, sid, "inv-probe")
	b2 := fetchFragment(t, srv, sid, "inv-probe")
	if b1 != b2 {
		t.Fatalf("live fragment HTML must be identical across two idle renders (server churn = guaranteed flicker)")
	}
	d1, _ := srv.investigationDetailData(ctx, "inv-probe")
	d2, _ := srv.investigationDetailData(ctx, "inv-probe")
	for _, k := range []string{"StatusHash", "TimelineHash", "SideHash"} {
		if d1[k] != d2[k] {
			t.Fatalf("%s must be stable across idle renders, got %v -> %v", k, d1[k], d2[k])
		}
	}

	// Confirm the badge really is present in this state (otherwise the client
	// assertion below would be vacuously true and the regression could slip in).
	if !strings.Contains(b1, `id="live-pulse"`) {
		t.Fatalf("awaiting-approval investigation must render #live-pulse (status active); fragment:\n%s", b1)
	}

	// (2) Client structural invariant: the live engine must NOT relabel the
	// badge on a routine poll. The label setter must be guarded so it only
	// writes on a real state change, and the in-flight path must not set
	// "updating" (the wider word that reflowed the header every ~4.5s).
	js, err := os.ReadFile(filepath.Join("static", "hub.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	// The label setter MUST be change-gated, so a steady-state poll is a no-op.
	if !strings.Contains(src, "text.textContent !== label") {
		t.Fatalf("hub.js liveLabel must guard the DOM write (only on change) so steady-state polls cause zero reflow")
	}
	// No code path may relabel the badge to the wider 'updating' word on a
	// routine poll (checked as call forms, immune to prose in comments).
	for _, bad := range []string{`liveLabel('updating')`, `liveLabel("updating")`, `pulse('updating')`, `pulse("updating")`} {
		if strings.Contains(src, bad) {
			t.Fatalf("hub.js must not relabel the live badge via %s on routine polls — that reflows the header (flicker)", bad)
		}
	}
	// The routine in-flight path must use the layout-neutral heartbeat.
	if !strings.Contains(src, "heartbeat()") {
		t.Fatalf("hub.js must signal a routine poll via the layout-neutral heartbeat(), not a label change")
	}
}
