package investigator

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

func TestIsContinueIntent(t *testing.T) {
	yes := []string{"continue", "Continue.", "продолжай", " ПРОДОЛЖАЙ ", "ok", "да", "proceed", "go on", "далее"}
	no := []string{"", "check the kernel ring instead", "look at /var/log/syslog", "грепай syslog по tpm"}
	for _, s := range yes {
		if !isContinueIntent(s) {
			t.Errorf("isContinueIntent(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isContinueIntent(s) {
			t.Errorf("isContinueIntent(%q) = true, want false", s)
		}
	}
}

func TestHandleMarkDone_InconclusiveAcceptedEmptyRejected(t *testing.T) {
	ctx := context.Background()
	// Inconclusive close (root_cause == "inconclusive") is accepted. An honest
	// inconclusive close still MUST carry observed symptoms and where_to_look_next
	// (the host-x-incident lesson: a symptom-less negative close is not a result).
	ok := handleMarkDone(ctx, HandlerEnv{}, `{"summary":{"root_cause":"inconclusive","confidence":"inconclusive","symptoms":["host unreachable over network"],"where_to_look_next":["switch-side LACP/port logs around the incident window"],"recommended_remediation":"none"}}`)
	if !ok.OK {
		t.Fatalf("inconclusive close must be accepted, got: %s", ok.Error)
	}
	// Empty root_cause is rejected with actionable guidance mentioning inconclusive.
	for _, bad := range []string{
		`{"summary":{"root_cause":"   ","recommended_remediation":"x"}}`,
		`{"summary":{"recommended_remediation":"x"}}`,
	} {
		res := handleMarkDone(ctx, HandlerEnv{}, bad)
		if res.OK {
			t.Fatalf("empty/missing root_cause must be rejected: %s", bad)
		}
		if !strings.Contains(res.Error, "inconclusive") {
			t.Fatalf("rejection must tell the model how to close inconclusively, got: %s", res.Error)
		}
	}
}

// A MUST-level operator directive issued after a load-bearing finding must lift
// the post-finding probe lockdown so the operator's directive is actionable.
func TestPostFindingRestricted_ResetByOperatorDirective(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-pf"
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: inv, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: "f1", InvestigationID: inv, Seq: 1, Tool: "add_finding",
		InputJSON: `{"severity":"warn","code":"tpm.spam","message":"m","evidence_refs":["t1","t2"]}`, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateToolCall(ctx, "f1", "executed", "auto", "", `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	// The finding's tool-result message (operatorDirectiveAfter anchors on it).
	if _, err := st.AppendMessage(ctx, store.Message{InvestigationID: inv, Role: "tool",
		Content: `{"ok":true}`, ToolCallID: sql.NullString{String: "f1", Valid: true}}); err != nil {
		t.Fatal(err)
	}

	l := &Loop{store: st}
	if !l.postFindingRestricted(ctx, inv) {
		t.Fatal("a load-bearing finding should restrict the tool list")
	}
	if _, err := st.AppendMessage(ctx, store.Message{InvestigationID: inv, Role: "user",
		Content: "OPERATOR RESUME [priority: HIGH]\nгрепай syslog по tpm"}); err != nil {
		t.Fatal(err)
	}
	if l.postFindingRestricted(ctx, inv) {
		t.Fatal("an operator directive after the finding must lift the restriction")
	}
}

// A rejected mark_done (empty root_cause) must NOT finalize the investigation;
// it stays active so the model can retry with a valid/inconclusive summary.
func TestExecuteApproved_RejectedMarkDoneDoesNotFinalize(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-md"
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: inv, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	input := `{"summary":{"recommended_remediation":"x"}}` // no root_cause
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: "m1", InvestigationID: inv, Seq: 1, Tool: "mark_done", InputJSON: input, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	l := &Loop{store: st, bus: NewBus()}
	tc := &store.ToolCallRow{ID: "m1", InvestigationID: inv, Tool: "mark_done", InputJSON: input}
	if err := l.executeApproved(ctx, inv, tc); err != nil {
		t.Fatalf("executeApproved: %v", err)
	}
	got, err := st.GetInvestigation(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "done" {
		t.Fatal("a rejected mark_done must not finalize the investigation")
	}
}
