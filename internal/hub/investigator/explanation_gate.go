package investigator

import (
	"context"
	"strings"
)

// explanationNudgeMarker is the stable sentence embedded in the explanatory-
// adequacy gate's mark_done rejection. Like coverageNudgeMarker it is the single
// source of truth for two coupled things:
//   - the one-time guard (a prior executed mark_done whose result carries it means
//     "already self-critiqued once — accept the next close"), and
//   - the post-finding lockdown lift (a bounced mark_done is the newest executed
//     tool, so restrict.go's postFindingRestricted re-offers probe tools for the
//     single re-plan turn — see that function's comment).
const explanationNudgeMarker = "explanation check: confirm the conclusion causally explains the primary symptom"

// explanationGateVerdict is the result of evaluateExplanationGate.
type explanationGateVerdict struct {
	bounce  bool
	message string
}

// evaluateExplanationGate is the hub-side backstop for prompt rules 14/15. Where
// the coverage gate (coverage_gate.go) checks the BREADTH of drilling, this gate
// checks the SYNTHESIS: before a CONFIDENT close it forces one self-critique turn
// that confirms the root_cause causally explains the PRIMARY symptom and that any
// symptom-matching observation the model itself surfaced was considered — not
// silently dropped. This is the inv_a00000000003 failure the coverage gate could
// not catch: the model drilled 22 regions (breadth satisfied), FOUND a confirmed
// NIC link-flap matching "lost network", then discarded it for not proving a
// "freeze" and closed on a negative ("no kernel hang") conclusion.
//
// It is deliberately coarse and cannot read the model's mind: it cannot prove a
// conclusion is wrong, only force the model to articulate the symptom→cause link
// once. It is one-time and operator-overridable (CLAUDE.md invariant 4), and
// fires ONLY for confident closes (confidence confirmed|likely); humble closes
// (speculative|inconclusive) already carry where_to_look_next and need no
// challenge. Computed entirely from ListToolCalls + the latest operator message;
// reads no artifact files.
func evaluateExplanationGate(ctx context.Context, env HandlerEnv, confidence string) explanationGateVerdict {
	if env.Store == nil || env.InvestigationID == "" {
		return explanationGateVerdict{} // best-effort: no store/context to evaluate against
	}
	conf := strings.ToLower(strings.TrimSpace(confidence))

	decision := "bounce"
	switch conf {
	case "confirmed", "likely":
		// challengeable — fall through to the one-time / operator-forced checks
	default:
		decision = "skip_not_confident" // speculative / inconclusive / unknown
	}

	if decision == "bounce" {
		tcs, err := env.Store.ListToolCalls(ctx, env.InvestigationID)
		if err != nil {
			return explanationGateVerdict{} // best-effort: never block a close on a store error
		}
		for _, t := range tcs {
			if t.Tool == "mark_done" && t.Status == "executed" &&
				t.ResultJSON.Valid && strings.Contains(t.ResultJSON.String, explanationNudgeMarker) {
				decision = "skip_already_nudged" // one-time guarantee — never an infinite block
				break
			}
		}
	}
	if decision == "bounce" && env.OperatorApprovedClose {
		// Explicit operator "Approve & close" is the authoritative close
		// (invariant 4) — never bounce the human's approval back to the model.
		decision = "skip_operator_approved"
	}
	if decision == "bounce" && operatorForcedClose(ctx, env) {
		decision = "skip_operator_forced"
	}

	if env.Log != nil {
		env.Log.Debug("explanation gate evaluated",
			"investigation_id", env.InvestigationID,
			"confidence", conf,
			"decision", decision)
	}

	if decision != "bounce" {
		return explanationGateVerdict{}
	}
	msg := explanationNudgeMarker + ". Before this confident close, do a ONE-turn self-critique (rule 14/15): " +
		"(1) restate the PRIMARY observed symptom; (2) state in one sentence how your root_cause CAUSALLY explains " +
		"that symptom — not merely what you ruled out; (3) list every observation in this investigation that ALSO " +
		"matches the primary symptom but you did NOT adopt as the cause, and for each give the evidence that rules " +
		"it out (if you cannot, that observation is the more likely cause — pivot to it and, once confirmed, cite its " +
		"task_id in the conclusion's evidence_refs); (4) if you cannot causally " +
		"explain the primary symptom, set confidence:\"inconclusive\" and fill where_to_look_next. The post-finding " +
		"probe lockdown is lifted for this one re-plan turn if you need a single confirming observation. Then call " +
		"mark_done again — it will be accepted."
	return explanationGateVerdict{bounce: true, message: msg}
}
