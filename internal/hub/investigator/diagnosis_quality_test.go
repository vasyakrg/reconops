package investigator

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// ---- explanatory-adequacy gate -------------------------------------------

// A CONFIDENT close is bounced once with the self-critique nudge; humble closes
// (speculative/inconclusive) are exempt; the bounce is one-time and stands down
// on an operator forced close.
func TestExplanationGate_BouncesConfidentCloseOnce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-expl"
	mustInsertInv(t, st, inv)

	for _, conf := range []string{"confirmed", "likely"} {
		v := evaluateExplanationGate(ctx, gateEnv(st, inv), conf)
		if !v.bounce {
			t.Fatalf("confidence %q must be bounced for self-critique", conf)
		}
		if !strings.Contains(v.message, explanationNudgeMarker) {
			t.Fatalf("bounce message must carry the explanation marker")
		}
	}
	for _, conf := range []string{"speculative", "inconclusive", ""} {
		if evaluateExplanationGate(ctx, gateEnv(st, inv), conf).bounce {
			t.Fatalf("confidence %q must NOT be bounced (humble close)", conf)
		}
	}
}

func TestExplanationGate_OneTimeAndOperatorOverride(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-expl-once"
	mustInsertInv(t, st, inv)
	// A prior executed mark_done carrying the nudge marker means "already
	// self-critiqued once" → stand down.
	insertExecutedTCWithResult(t, st, inv, "m1", "mark_done",
		`{"summary":{"root_cause":"x"}}`, `{"ok":false,"error":"`+explanationNudgeMarker+` ..."}`, 1)
	if evaluateExplanationGate(ctx, gateEnv(st, inv), "confirmed").bounce {
		t.Fatal("a second confident close after one nudge must be accepted")
	}

	const inv2 = "inv-expl-op"
	mustInsertInv(t, st, inv2)
	mustAppendUser(t, st, inv2, "OPERATOR FINALIZE [priority: HIGH]\nclose now")
	if evaluateExplanationGate(ctx, gateEnv(st, inv2), "confirmed").bounce {
		t.Fatal("OPERATOR FINALIZE must stand the explanation gate down")
	}
}

// ---- mark_done structured-conclusion validation --------------------------

// HandlerEnv{} has a nil store, so both gates no-op (best-effort) and only the
// field validation runs — exactly what we want to assert here.
func TestMarkDone_StructuredValidation(t *testing.T) {
	ctx := context.Background()
	ok := func(j string) ToolResult { return handleMarkDone(ctx, HandlerEnv{}, j) }

	// Complete confident ("likely") close is accepted.
	good := `{"summary":{"symptoms":["unreachable over network"],"root_cause":"nic flap","root_cause_explains":"unreachable over network","confidence":"likely","where_to_look_next":["switch logs — port counters"],"recommended_remediation":"check the port"}}`
	if r := ok(good); !r.OK {
		t.Fatalf("complete likely close must be accepted, got: %s", r.Error)
	}
	// A confirmed close needs no where_to_look_next.
	confirmed := `{"summary":{"symptoms":["s"],"root_cause":"rc","root_cause_explains":"s","confidence":"confirmed","recommended_remediation":"x"}}`
	if r := ok(confirmed); !r.OK {
		t.Fatalf("confirmed close without where_to_look_next must be accepted, got: %s", r.Error)
	}

	type badCase struct{ name, json, wantSub string }
	for _, c := range []badCase{
		{"missing symptoms", `{"summary":{"root_cause":"rc","confidence":"likely","root_cause_explains":"s","where_to_look_next":["w"],"recommended_remediation":"x"}}`, "symptoms"},
		{"missing confidence", `{"summary":{"symptoms":["s"],"root_cause":"rc","recommended_remediation":"x"}}`, "confidence"},
		{"missing root_cause_explains", `{"summary":{"symptoms":["s"],"root_cause":"rc","confidence":"likely","where_to_look_next":["w"],"recommended_remediation":"x"}}`, "root_cause_explains"},
		{"missing where_to_look_next when not confirmed", `{"summary":{"symptoms":["s"],"root_cause":"rc","confidence":"likely","root_cause_explains":"s","recommended_remediation":"x"}}`, "where_to_look_next"},
	} {
		r := ok(c.json)
		if r.OK {
			t.Fatalf("%s: must be rejected", c.name)
		}
		if !strings.Contains(r.Error, c.wantSub) {
			t.Fatalf("%s: error must mention %q, got: %s", c.name, c.wantSub, r.Error)
		}
	}
}

// ---- system prompt -------------------------------------------------------

func TestSystemPrompt_SymptomAnchoringRules(t *testing.T) {
	p := BuildSystemPrompt("host unreachable", "m", time.Now(), 12, 500000)
	for _, want := range []string{
		"Frame the incident by observable symptoms",
		"causally explain the PRIMARY",
		"MUST NOT be discarded merely because",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("system prompt missing rule fragment: %q", want)
		}
	}
}

// ---- differential re-rank checkpoint -------------------------------------

func countRerankNotes(t *testing.T, st *store.Store, inv string) int {
	t.Helper()
	msgs, err := st.ListMessages(context.Background(), inv, true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.Role == "system_note" && strings.Contains(m.Content, rerankCheckpointMarker) {
			n++
		}
	}
	return n
}

func TestRerankCheckpoint_FiresPerIntervalUntilFinding(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-rerank"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st, rerankIntervalSteps: 2}

	// 0 probes → no checkpoint.
	l.maybeInjectRerankCheckpoint(ctx, inv)
	if n := countRerankNotes(t, st, inv); n != 0 {
		t.Fatalf("no checkpoint expected before any probes, got %d", n)
	}
	// 2 probes → first checkpoint.
	insertExecutedTC(t, st, inv, "c1", "collect", `{"collector":"x","host_id":"h"}`, 1)
	insertExecutedTC(t, st, inv, "c2", "search_artifact", `{"task_id":"t","pattern":"p"}`, 2)
	l.maybeInjectRerankCheckpoint(ctx, inv)
	if n := countRerankNotes(t, st, inv); n != 1 {
		t.Fatalf("expected 1 checkpoint at 2 probes, got %d", n)
	}
	// Still 2 probes → no second checkpoint (one per interval block).
	l.maybeInjectRerankCheckpoint(ctx, inv)
	if n := countRerankNotes(t, st, inv); n != 1 {
		t.Fatalf("checkpoint must fire once per interval, got %d", n)
	}
	// 4 probes → second checkpoint.
	insertExecutedTC(t, st, inv, "c3", "collect", `{"collector":"x","host_id":"h"}`, 3)
	insertExecutedTC(t, st, inv, "c4", "get_full_result", `{"task_id":"t2"}`, 4)
	l.maybeInjectRerankCheckpoint(ctx, inv)
	if n := countRerankNotes(t, st, inv); n != 2 {
		t.Fatalf("expected 2 checkpoints at 4 probes, got %d", n)
	}
	// A load-bearing finding suppresses further checkpoints (rule 9 owns that phase).
	insertExecutedTC(t, st, inv, "f1", "add_finding",
		`{"severity":"warn","code":"c","message":"m","evidence_refs":["t1","t2"]}`, 5)
	insertExecutedTC(t, st, inv, "c5", "collect", `{"collector":"x","host_id":"h"}`, 6)
	insertExecutedTC(t, st, inv, "c6", "collect", `{"collector":"x","host_id":"h"}`, 7)
	l.maybeInjectRerankCheckpoint(ctx, inv)
	if n := countRerankNotes(t, st, inv); n != 2 {
		t.Fatalf("no checkpoint after a load-bearing finding, got %d", n)
	}
}

// The maxRerankInjections cap is the only safeguard against the re-rank
// mechanism becoming its own loop fuel — drive past it with no finding and
// assert the count stalls at the cap (the suppression here is the cap, NOT a
// load-bearing finding).
func TestRerankCheckpoint_CapStopsAtMax(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-rerank-cap"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st, rerankIntervalSteps: 1} // one checkpoint per probe
	// Run well past maxRerankInjections probes, no load-bearing finding ever.
	for i := 1; i <= maxRerankInjections+3; i++ {
		insertExecutedTC(t, st, inv, "c"+itoa(i), "collect", `{"collector":"x","host_id":"h"}`, i)
		l.maybeInjectRerankCheckpoint(ctx, inv)
	}
	if n := countRerankNotes(t, st, inv); n != maxRerankInjections {
		t.Fatalf("checkpoint count must cap at %d, got %d", maxRerankInjections, n)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestRerankCheckpoint_DisabledWhenZeroInterval(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-rerank-off"
	mustInsertInv(t, st, inv)
	l := &Loop{store: st, rerankIntervalSteps: 0}
	insertExecutedTC(t, st, inv, "c1", "collect", `{"collector":"x","host_id":"h"}`, 1)
	insertExecutedTC(t, st, inv, "c2", "collect", `{"collector":"x","host_id":"h"}`, 2)
	l.maybeInjectRerankCheckpoint(ctx, inv)
	if n := countRerankNotes(t, st, inv); n != 0 {
		t.Fatalf("rerankIntervalSteps=0 must disable the checkpoint, got %d", n)
	}
}

// ---- priors fencing ------------------------------------------------------

func TestPriorsDigest_SurfacesIncidentAndFencingHeader(t *testing.T) {
	if !strings.Contains(priorsSeedHeader, "MUST") ||
		!strings.Contains(priorsSeedHeader, "do NOT adopt") ||
		!strings.Contains(priorsSeedHeader, "re-deriving") {
		t.Fatal("priors header must carry the MUST-level re-derive fence")
	}
	digest, rendered := RenderPriorsDigest([]PriorRecord{{
		ID:        "inv_prior1",
		Goal:      "host froze, console spam tpm error -62",
		Status:    "done",
		CreatedAt: time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC),
		Hosts:     []string{"host-x"},
		RootCause: "TPM timeout storm",
	}}, DefaultPriorsConfig())
	if len(rendered) != 1 {
		t.Fatalf("expected 1 rendered prior, got %d", len(rendered))
	}
	if !strings.Contains(digest, "its incident:") || !strings.Contains(digest, "console spam tpm error -62") {
		t.Fatalf("digest must surface the prior's incident/symptom, got:\n%s", digest)
	}
	if !strings.Contains(digest, "its conclusion:") || !strings.Contains(digest, "TPM timeout storm") {
		t.Fatalf("digest must surface the prior's conclusion, got:\n%s", digest)
	}
}
