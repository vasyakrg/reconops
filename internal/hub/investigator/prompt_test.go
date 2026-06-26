package investigator

import (
	"testing"
	"time"
)

// The Task 9 overhaul must add the time-anchor / work-backwards / journald
// fallback guidance AND must not weaken the MUST invariants.
func TestSystemPromptLogRetrievalGuidance(t *testing.T) {
	out := BuildSystemPrompt("g", "m", time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC), 40, 500_000)
	mustContain := []string{
		// Task 9 additions
		"uptime_sec",     // derive boot time from the host fact
		"collected_at",   // anchor on collection time, not "now"
		"from_end",       // read the tail / work backwards
		"previous_boot",  // pre-reboot logs
		"log_search",     // grep at source
		"4 MiB",          // search_artifact scan-cap warning
		"work backwards", // explicit technique
		"empty journal",  // journald-volatile fallback
		// MUST invariants that must survive the overhaul
		"ONE tool call per turn",
		"evidence_refs",
		"OPERATOR HYPOTHESIS",
		"Read-only.",
	}
	for _, want := range mustContain {
		if !contains(out, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if contains(out, "{{") {
		t.Error("placeholder left unsubstituted")
	}
}

// T10: prompt-clarity hardening — evidence_refs is task-ids-only (the schema +
// handlers.go guard reject anything else), the load-bearing definition is
// explicit, rule 7 surfaces the re-retrieval obligation, and the priors digest
// is tagged so cross-investigation hints aren't mistaken for current evidence.
func TestSystemPromptT10Clarifications(t *testing.T) {
	out := BuildSystemPrompt("g", "m", time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC), 40, 500_000)
	for _, want := range []string{
		"task_ids ONLY",                   // rule 3: evidence_refs accepts task_ids only
		"NOT evidence_refs values",        // rule 3: memory_id/finding_id are not refs
		"load-bearing** when BOTH",        // rule 9: explicit load-bearing definition
		"Re-retrieval",                    // rule 7: re-retrieval obligation surfaced
		"page through it with the offset", // rule 7: aligned with T7 windowing
	} {
		if !contains(out, want) {
			t.Errorf("system prompt missing T10 clarification %q", want)
		}
	}
	// The cross-investigation priors digest must carry its distinguishing marker.
	if !contains(priorsSeedHeader, "[CROSS_INVESTIGATION_HINT]") {
		t.Error("priors seed header missing the [CROSS_INVESTIGATION_HINT] marker")
	}
}

// HF1: the differential-methodology rule (rule 14) must add hypothesis-class
// enumeration, the "loudest != cause" / rare-outlier guidance, the
// budget-collapse handling, and the evidence_gap reconciliation with rules 4/9
// — without weakening the existing invariants asserted above.
func TestSystemPromptDifferentialGuidance(t *testing.T) {
	out := BuildSystemPrompt("g", "m", time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC), 40, 500_000)
	mustContain := []string{
		"candidate root-cause class",    // enumerate candidate classes from the symptom
		"loudest",                       // the loudest cluster is not the default cause
		"outlier",                       // rare/outlier clusters deserve scrutiny
		"_index_truncated",              // budget-collapse: re-read the full index
		"get_full_result(task_id)",      // ... via get_full_result over the FULL cluster set
		"BEFORE a load-bearing finding", // coverage reconciled with rules 4 & 9 (pre-finding)
		"rule 14",                       // cross-referenced from rules 4 and 7
	}
	for _, want := range mustContain {
		if !contains(out, want) {
			t.Errorf("system prompt missing differential guidance %q", want)
		}
	}
}
