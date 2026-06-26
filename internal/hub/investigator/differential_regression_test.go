package investigator

import (
	"context"
	"strings"
	"testing"
)

// TestUnreachableHostRegression is the DETERMINISTIC, CI-run regression for the
// inv_a00000000003 failure class — no LLM, scripted payloads. It encodes the
// lesson abstractly: a conclusion that only rules things out and drops the
// confirmed symptom-matching observation must not be accepted as a confident
// close; the conclusion that explains the PRIMARY symptom must be (after one
// self-critique turn). The companion live, non-deterministic eval lives in
// differential_eval_test.go behind the `eval` build tag.
//
// Failure shape (replayed from the real session): primary symptom = "host
// unreachable over network"; the model found a NIC/LACP carrier flap that
// matched it, then discarded it for not proving a "freeze" and closed on a
// negative "no kernel hang" conclusion. The hardened mark_done contract + the
// explanatory-adequacy gate are what now block that shape.
func TestUnreachableHostRegression(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-host-x-regress"
	mustInsertInv(t, st, inv)
	env := gateEnv(st, inv)

	// (1) The original close: a negative, symptom-less conclusion (no confidence,
	// no symptoms, no symptom-explanation) that silently dropped the NIC flap.
	// The structured-conclusion validation rejects it outright.
	original := `{"summary":{` +
		`"root_cause":"OS shows no kernel panic/lockup/OOM; main failure is pvedaemon ipcc_send_rec during snapshot burst",` +
		`"recommended_remediation":"separate the TPM and snapshot issues"}}`
	if r := handleMarkDone(ctx, env, original); r.OK {
		t.Fatal("the original symptom-less negative close must be rejected by the structured-conclusion gate")
	}

	// (2) A field-complete CONFIDENT close that DOES explain the primary symptom
	// passes field validation, but is bounced exactly once for the explanatory
	// self-critique — the turn that, in the real run, would have resurfaced the
	// discarded NIC lead instead of letting it evaporate.
	good := `{"summary":{` +
		`"symptoms":["host unreachable over network","IPMI console unresponsive except Ctrl+Alt+Del"],` +
		`"root_cause":"carrier flap on LACP member eno1 dropped the management uplink (vmbr1 over bond0.1)",` +
		`"root_cause_explains":"host unreachable over network",` +
		`"confidence":"likely",` +
		`"evidence_refs":["t-kernel","t-ifaces"],` +
		`"where_to_look_next":["switch-side LACP/port-channel logs around the incident window"],` +
		`"recommended_remediation":"check the eno1 switch port, cable/SFP, and LACP partner state"}}`
	r := handleMarkDone(ctx, env, good)
	if r.OK {
		t.Fatal("the first confident close must be bounced once for the explanatory self-critique")
	}
	if !strings.Contains(r.Error, explanationNudgeMarker) {
		t.Fatalf("the bounce must be the explanation-gate nudge, got: %s", r.Error)
	}

	// The loop records a bounced mark_done as an executed tool carrying the nudge
	// (executeApproved path); replay that so the one-time guard fires next turn.
	insertExecutedTCWithResult(t, st, inv, "md-bounce", "mark_done", good,
		`{"ok":false,"error":"`+explanationNudgeMarker+` ..."}`, 10)

	// (3) On the re-plan turn the same symptom-explaining conclusion is accepted —
	// the self-critique is one-time, never an infinite block.
	if r := handleMarkDone(ctx, env, good); !r.OK {
		t.Fatalf("after the one self-critique, the symptom-explaining close must be accepted, got: %s", r.Error)
	}
}
