package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// A non-armed, non-terminal investigation renders the single ⚙ Automation
// popover: both modes (bounded "Run autonomously" arm form + unbounded ⚡ auto
// toggle) live behind it, each scoped to THIS investigation.
func TestAutonomousControlRendersArmForm(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-auton-arm", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := fetchFragment(t, srv, sid, "inv-auton-arm")
	for _, want := range []string{
		`class="auto-pop"`,        // the single unified automation control
		"⚙ Automation",            // the popover trigger
		"this investigation only", // per-investigation scope is made explicit
		`action="/investigations/autonomous"`,
		`name="action" value="arm"`,
		`name="steps"`,
		`name="tokens"`,
		"Run autonomously", // bounded mode (recommended)
		`action="/investigations/auto-approve"`,
		"⚡ auto", // the unbounded toggle is the second mode inside the popover
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("automation popover missing %q in rendered fragment", want)
		}
	}
}

// When an autonomous burst is armed the header shows a legible armed status plus
// a "take over" (disarm) control, and the ⚙ Automation popover / unbounded ⚡
// auto toggle are hidden (the bounded run supersedes them).
func TestAutonomousControlRendersTakeOverWhenArmed(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-auton-on", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAutonomousRun(ctx, "inv-auton-on", 18, 300_000); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := fetchFragment(t, srv, sid, "inv-auton-on")
	for _, want := range []string{
		`name="action" value="disarm"`,
		"take over",
		"pauses for review", // the armed status is legible, not a cryptic pill
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("armed run must render the disarm/take-over control with %q", want)
		}
	}
	if strings.Contains(body, "⚡ auto") {
		t.Fatalf("the unbounded ⚡ auto toggle must be hidden while an autonomous burst is armed")
	}
	if strings.Contains(body, "⚙ Automation") {
		t.Fatalf("the Automation popover must be hidden while an autonomous burst is armed")
	}
}
