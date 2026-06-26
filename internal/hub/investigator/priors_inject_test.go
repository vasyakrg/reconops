package investigator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// seedDonePrior inserts a completed investigation on the given hosts with a
// root-cause summary, so a later Start on overlapping hosts can pick it up.
func seedDonePrior(t *testing.T, st *store.Store, id string, hosts []string, rootCause string) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: id, Goal: "g-" + id, Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u", AllowedHosts: hosts,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, id, "done",
		store.TerminalDonePayload(`{"root_cause":"`+rootCause+`"}`, time.Time{}).JSON()); err != nil {
		t.Fatal(err)
	}
}

func priorsSeedFor(t *testing.T, st *store.Store, invID string) (string, int) {
	t.Helper()
	msgs, err := st.ListMessages(context.Background(), invID, true)
	if err != nil {
		t.Fatal(err)
	}
	systemSeeds := 0
	seed := ""
	for _, m := range msgs {
		if m.Role == "system" {
			systemSeeds++
			if strings.Contains(m.Content, "Prior investigations (read-only context)") {
				seed = m.Content
			}
		}
	}
	return seed, systemSeeds
}

// Start injects a compact priors digest (header + disclaimer + prior root cause)
// as a system seed BEFORE the user goal, for a prior on an overlapping host.
func TestStart_InjectsPriorsDigestForOverlappingHost(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedDonePrior(t, st, "prior-1", []string{"web01"}, "nginx OOM under burst")

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)
	l.SetPriorsConfig(DefaultPriorsConfig())

	id, err := l.Start(ctx, "investigate web01 latency", "operator", "", "web01")
	if err != nil {
		t.Fatal(err)
	}
	waitPending(t, st, id)

	seed, _ := priorsSeedFor(t, st, id)
	if seed == "" {
		t.Fatal("Start must inject a prior-investigations system seed for an overlapping host")
	}
	if !strings.Contains(seed, "nginx OOM under burst") {
		t.Fatalf("priors seed must carry the prior root cause, got: %s", seed)
	}
	if !strings.Contains(seed, "HINTS") {
		t.Fatalf("priors seed must carry the re-verify disclaimer, got: %s", seed)
	}

	// The priors seed must precede the user goal in the message history.
	msgs, _ := st.ListMessages(ctx, id, true)
	priorsIdx, goalIdx := -1, -1
	for i, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "Prior investigations") && priorsIdx == -1 {
			priorsIdx = i
		}
		if m.Role == "user" && m.Content == "investigate web01 latency" && goalIdx == -1 {
			goalIdx = i
		}
	}
	if priorsIdx == -1 || goalIdx == -1 || priorsIdx >= goalIdx {
		t.Fatalf("priors seed must precede the user goal (priors@%d, goal@%d)", priorsIdx, goalIdx)
	}
}

func TestStart_NoPriorsWhenDisabled(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedDonePrior(t, st, "prior-1", []string{"web01"}, "nginx OOM")

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)
	cfg := DefaultPriorsConfig()
	cfg.Enabled = false
	l.SetPriorsConfig(cfg)

	id, err := l.Start(ctx, "investigate web01", "operator", "", "web01")
	if err != nil {
		t.Fatal(err)
	}
	waitPending(t, st, id)
	if seed, _ := priorsSeedFor(t, st, id); seed != "" {
		t.Fatal("priors disabled must inject no priors seed")
	}
}

func TestStart_NoPriorsWhenNoHostOverlap(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedDonePrior(t, st, "prior-1", []string{"db01"}, "pg autovacuum storm")

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)
	l.SetPriorsConfig(DefaultPriorsConfig()) // host_overlap scope

	id, err := l.Start(ctx, "investigate web01", "operator", "", "web01")
	if err != nil {
		t.Fatal(err)
	}
	waitPending(t, st, id)
	if seed, _ := priorsSeedFor(t, st, id); seed != "" {
		t.Fatal("host_overlap scope must inject nothing when no prior shares a host")
	}
}

// An operator-selected prior is injected even when it shares NO host with the
// new investigation (manual overrides the host-overlap filter), and the
// attached prior ids are recorded on the investigation for the detail page.
func TestStartWithPriors_IncludesManualNonOverlappingHost(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedDonePrior(t, st, "prior-db", []string{"db01"}, "pg autovacuum storm")

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)
	l.SetPriorsConfig(DefaultPriorsConfig()) // host_overlap scope

	// New investigation on web01 (no overlap with db01), but operator picks the
	// db01 prior explicitly.
	id, err := l.StartWithPriors(ctx, "investigate web01", "operator", "", []string{"prior-db"}, []string{"web01"})
	if err != nil {
		t.Fatal(err)
	}
	waitPending(t, st, id)

	seed, _ := priorsSeedFor(t, st, id)
	if seed == "" || !strings.Contains(seed, "pg autovacuum storm") {
		t.Fatalf("a manually selected prior must be injected even without host overlap, got: %q", seed)
	}
	inv, err := st.GetInvestigation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range inv.Priors {
		if p == "prior-db" {
			found = true
		}
	}
	if !found {
		t.Fatalf("attached priors must be recorded on the investigation, got %+v", inv.Priors)
	}
}

// A manually-attached ABORTED prior (no final conclusion) is injected using its
// findings, with a status tag, and recorded on the investigation.
func TestStartWithPriors_ManualAbortedUsesFindings(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "prior-ab", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u", AllowedHosts: []string{"db01"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFinding(ctx, store.Finding{
		ID: "f1", InvestigationID: "prior-ab", Severity: "error", Code: "pg.vacuum", Message: "autovacuum stuck on big table",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "prior-ab", "aborted", `{"kind":"aborted","reason":"operator ended"}`); err != nil {
		t.Fatal(err)
	}

	router, closeSrv := askOperatorLLM(t)
	t.Cleanup(closeSrv)
	l := newRetryLoop(st, router)
	l.SetPriorsConfig(DefaultPriorsConfig())

	id, err := l.StartWithPriors(ctx, "investigate web01", "operator", "", []string{"prior-ab"}, []string{"web01"})
	if err != nil {
		t.Fatal(err)
	}
	waitPending(t, st, id)

	seed, _ := priorsSeedFor(t, st, id)
	if seed == "" || !strings.Contains(seed, "prior-ab") || !strings.Contains(seed, "autovacuum stuck") {
		t.Fatalf("manual aborted prior must be injected with its findings, got: %q", seed)
	}
	if !strings.Contains(seed, "[aborted]") {
		t.Fatalf("a non-done prior must carry a status tag, got: %q", seed)
	}
	inv, err := st.GetInvestigation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range inv.Priors {
		if p == "prior-ab" {
			found = true
		}
	}
	if !found {
		t.Fatalf("attached aborted prior must be recorded, got %+v", inv.Priors)
	}
}
