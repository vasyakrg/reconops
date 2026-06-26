package investigator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// coverageNudgeMarker is the stable sentence embedded in the coverage gate's
// mark_done rejection. It is the single source of truth for three things that
// must stay in lockstep:
//   - the operator-/model-facing nudge text the model reads on the bounce,
//   - the one-time guard (a prior executed mark_done whose result carries it
//     means "already nudged once — accept the next close"), and
//   - the post-finding lockdown interaction (a bounced mark_done becomes the
//     newest executed tool, so restrict.go's postFindingRestricted naturally
//     re-offers probe tools for the re-plan turn — see that function's comment).
const coverageNudgeMarker = "coverage gap: high-prior candidate root-cause classes look unscrutinised"

// minDrillsForCoverage is the number of DISTINCT log drill-downs
// (search_artifact signatures + get_full_result task_ids) at or above which a
// multi-cluster index is treated as actually explored rather than anchored-on.
// Two means the model looked past the single dominant cluster at least once.
const minDrillsForCoverage = 2

// coverageGateVerdict is the result of evaluateCoverageGate.
type coverageGateVerdict struct {
	bounce  bool
	message string
}

// evaluateCoverageGate is the hub-side backstop for prompt rule 14. It bounces
// the FIRST mark_done of an investigation that concluded while a multi-cluster
// log index was on offer but the model barely drilled it — the inv_a00000000002
// failure: anchored on the loudest TPM cluster and called mark_done, never
// scrutinising the rare NIC/link-flap clusters that were already in the
// artifact_index.
//
// It is deliberately a COARSE breadth proxy, not per-class coverage: there is
// no structured candidate-class state in tool history to check against
// (store.MemoryKindHypothesis has no write sites; collect input carries only a
// free-form collector string). The precise per-class discipline lives in the
// system prompt (rule 14); this gate only catches the gross "anchored and
// bailed" shape, and only once. The signal is computed entirely from
// ListToolCalls (each row carries the stored tool result), so no artifact files
// are read here.
//
// Returns bounce=false (let mark_done finalize) when any of:
//   - no multi-cluster breadth was ever on offer (nothing to explore), or the
//     model already drilled >= minDrillsForCoverage distinct regions;
//   - it has already been nudged once this investigation (one-time guarantee —
//     never an infinite block, respects the anti-loop budget);
//   - the operator explicitly approved/edited THIS close (the "Approve & close"
//     button — env.OperatorApprovedClose). An explicit human approval is the
//     authoritative close and must not be bounced back to the model;
//   - the operator explicitly forced the close (OPERATOR FINALIZE / HYPOTHESIS
//     / RESUME as the latest operator message — CLAUDE.md invariant 4).
func evaluateCoverageGate(ctx context.Context, env HandlerEnv) coverageGateVerdict {
	if env.Store == nil || env.InvestigationID == "" {
		return coverageGateVerdict{} // best-effort: no store/context to evaluate against
	}
	tcs, err := env.Store.ListToolCalls(ctx, env.InvestigationID)
	if err != nil {
		return coverageGateVerdict{} // best-effort: never block a close on a store error
	}

	breadthAvailable := false
	drills := map[string]struct{}{}
	alreadyNudged := false
	for _, t := range tcs {
		if t.Status != "executed" {
			continue
		}
		switch t.Tool {
		case "collect", "collect_batch":
			if t.ResultJSON.Valid && resultHasMultiCluster(t.ResultJSON.String) {
				breadthAvailable = true
			}
		case "search_artifact":
			drills[searchSignature(t.InputJSON)] = struct{}{}
		case "get_full_result":
			drills[fullResultSignature(t.InputJSON)] = struct{}{}
		case "mark_done":
			if t.ResultJSON.Valid && strings.Contains(t.ResultJSON.String, coverageNudgeMarker) {
				alreadyNudged = true
			}
		}
	}

	decision := "bounce"
	switch {
	case env.OperatorApprovedClose:
		decision = "skip_operator_approved"
	case alreadyNudged:
		decision = "skip_already_nudged"
	case !breadthAvailable:
		decision = "skip_no_breadth"
	case len(drills) >= minDrillsForCoverage:
		decision = "skip_sufficient_breadth"
	case operatorForcedClose(ctx, env):
		decision = "skip_operator_forced"
	}

	if env.Log != nil {
		env.Log.Debug("coverage gate evaluated",
			"investigation_id", env.InvestigationID,
			"breadth_available", breadthAvailable,
			"distinct_drills", len(drills),
			"already_nudged", alreadyNudged,
			"decision", decision)
	}

	if decision != "bounce" {
		return coverageGateVerdict{}
	}
	msg := fmt.Sprintf("%s. You are concluding after drilling only %d distinct log region(s) "+
		"while a multi-cluster artifact_index was surfaced. Re-plan (rule 14): enumerate the "+
		"candidate root-cause classes from the symptom, and for each high-prior class either run "+
		"its single cheapest discriminating probe over the FULL cluster set (get_full_result(task_id) — "+
		"if it returns a bounded window, page the rest with its offset argument — then a class-specific "+
		"search_artifact) — the loudest cluster is one candidate, not the default "+
		"cause — or record it as explicitly unchecked-with-reason. The post-finding probe lockdown is "+
		"lifted for this one re-plan turn. Then call mark_done again with the completed differential; "+
		"it will be accepted.", coverageNudgeMarker, len(drills))
	return coverageGateVerdict{bounce: true, message: msg}
}

// resultHasMultiCluster reports whether a stored collect / collect_batch tool
// result exposed a log artifact_index with breadth to explore: a per-task index
// with >= 2 clusters, an index the budget collapsed to a headline
// (_index_truncated — collapse only happens when there were more clusters), or a
// cross-host batch roll-up with >= 2 clusters.
func resultHasMultiCluster(resultJSON string) bool {
	if resultJSON == "" {
		return false
	}
	var res struct {
		Data struct {
			Tasks []struct {
				ArtifactIndex []struct {
					TopPatterns []json.RawMessage `json:"top_patterns"`
				} `json:"artifact_index"`
				IndexTruncated bool `json:"_index_truncated"`
			} `json:"tasks"`
			BatchRollup struct {
				Clusters []json.RawMessage `json:"clusters"`
			} `json:"batch_rollup"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &res); err != nil {
		return false
	}
	if len(res.Data.BatchRollup.Clusters) >= 2 {
		return true
	}
	for _, t := range res.Data.Tasks {
		if t.IndexTruncated {
			return true
		}
		for _, ai := range t.ArtifactIndex {
			if len(ai.TopPatterns) >= 2 {
				return true
			}
		}
	}
	return false
}

// searchSignature canonicalizes a search_artifact input into a distinct-region
// key. Best-effort: a malformed input collapses to a stable empty signature.
func searchSignature(inputJSON string) string {
	var a struct {
		TaskID       string `json:"task_id"`
		ArtifactName string `json:"artifact_name"`
		Pattern      string `json:"pattern"`
	}
	_ = json.Unmarshal([]byte(inputJSON), &a)
	return "s:" + a.TaskID + "|" + a.ArtifactName + "|" + a.Pattern
}

// fullResultSignature canonicalizes a get_full_result input into a
// distinct-region key (one per task_id).
func fullResultSignature(inputJSON string) string {
	var a struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal([]byte(inputJSON), &a)
	return "f:" + a.TaskID
}

// operatorForcedClose reports whether the most recent operator (user) message
// is a MUST-level directive that forces a close or redirect — in which case the
// coverage gate must stand down (the operator outranks the differential
// backstop; CLAUDE.md invariant 4). Mirrors the role/marker discipline of
// restrict.go operatorDirectiveAfter.
func operatorForcedClose(ctx context.Context, env HandlerEnv) bool {
	msgs, err := env.Store.ListMessages(ctx, env.InvestigationID, true)
	if err != nil {
		return false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		c := msgs[i].Content
		return strings.Contains(c, "OPERATOR FINALIZE") ||
			strings.Contains(c, "OPERATOR HYPOTHESIS") ||
			strings.Contains(c, "OPERATOR RESUME")
	}
	return false
}
