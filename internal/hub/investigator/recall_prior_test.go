package investigator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// recall_prior lets the model pull a referenced prior's FULL conclusion +
// findings (undistorted) instead of re-collecting from the host — the gap that
// made models "not see" attached prior investigations.
func TestRecallPrior_DoneSurfacesConclusionAndFindings(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv_prior1", Goal: "напиши конфиг исправляющий проблемы", Status: "active",
		CreatedBy: "o", Model: "m", BaseURL: "u", AllowedHosts: []string{"host-x"},
	}); err != nil {
		t.Fatal(err)
	}
	summary := `{"root_cause":"bond0 had no bond-min-links",` +
		`"recommended_remediation":"set bond-min-links 2 in /etc/network/interfaces",` +
		`"confidence":"likely","symptoms":["network blackhole"]}`
	if err := st.FinishInvestigation(ctx, "inv_prior1", "done",
		store.TerminalDonePayload(summary, time.Now().UTC()).JSON()); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFinding(ctx, store.Finding{
		ID: "f1", InvestigationID: "inv_prior1", Severity: "warn", Code: "network.lacp",
		Message: "single-flow blackhole risk", EvidenceJSON: `{"task_ids":["task_x"]}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Gating: a prior NOT attached to this run is refused (no fishing).
	notAttached := handleRecallPrior(ctx,
		HandlerEnv{Store: st, InvestigationID: "inv_new"},
		`{"investigation_id":"inv_prior1"}`)
	if notAttached.OK {
		t.Fatal("recall of a non-attached prior must be refused")
	}

	env := HandlerEnv{Store: st, InvestigationID: "inv_new", AttachedPriors: []string{"inv_prior1"}, MaxResultTokens: 2000}
	res := handleRecallPrior(ctx, env, `{"investigation_id":"inv_prior1"}`)
	if !res.OK {
		t.Fatalf("recall failed: %s", res.Error)
	}
	body, _ := json.Marshal(res.Data)
	for _, want := range []string{
		"set bond-min-links 2 in /etc/network/interfaces", // remediation surfaced verbatim
		"single-flow blackhole risk",                      // finding message untruncated
		"network.lacp",
		"final conclusion (done)",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("recall result missing %q in:\n%s", want, body)
		}
	}
}

// For a prior that never finalized, recall_prior surfaces the latest mark_done
// PROPOSAL — that is where "write me that config again" usually lives.
func TestRecallPrior_NonDoneSurfacesProposal(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv_p2", Goal: "g", Status: "waiting", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc1", InvestigationID: "inv_p2", Seq: 5, Tool: "mark_done",
		InputJSON: `{"summary":{"root_cause":"tpm storm","recommended_remediation":"disable tpm in firmware"}}`,
		Status:    "pending",
	}); err != nil {
		t.Fatal(err)
	}
	env := HandlerEnv{Store: st, InvestigationID: "inv_new", AttachedPriors: []string{"inv_p2"}, MaxResultTokens: 2000}
	res := handleRecallPrior(ctx, env, `{"investigation_id":"inv_p2"}`)
	if !res.OK {
		t.Fatalf("recall failed: %s", res.Error)
	}
	body, _ := json.Marshal(res.Data)
	if !strings.Contains(string(body), "disable tpm in firmware") || !strings.Contains(string(body), "not finalized") {
		t.Fatalf("non-done recall must surface the proposed conclusion:\n%s", body)
	}
}
