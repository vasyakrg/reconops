package investigator

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

func donePrior(id string, hosts []string, rootCause string, createdAt time.Time) store.PriorInvestigation {
	summary := store.TerminalDonePayload(`{"root_cause":"`+rootCause+`"}`, time.Time{}).JSON()
	return store.PriorInvestigation{
		ID: id, Goal: "g-" + id, CreatedAt: createdAt, AllowedHosts: hosts,
		SummaryJSON: sql.NullString{String: summary, Valid: true},
	}
}

func TestPriorRootCause(t *testing.T) {
	good := store.TerminalDonePayload(`{"root_cause":"tpm storm"}`, time.Time{}).JSON()
	if rc := priorRootCause(sql.NullString{String: good, Valid: true}); rc != "tpm storm" {
		t.Fatalf("want 'tpm storm', got %q", rc)
	}
	inc := store.TerminalDonePayload(`{"root_cause":"inconclusive"}`, time.Time{}).JSON()
	if rc := priorRootCause(sql.NullString{String: inc, Valid: true}); rc != "" {
		t.Fatalf("inconclusive close must be dropped, got %q", rc)
	}
	if rc := priorRootCause(sql.NullString{Valid: false}); rc != "" {
		t.Fatalf("missing/legacy summary must be dropped, got %q", rc)
	}
	bare := store.TerminalDonePayload(`{}`, time.Time{}).JSON() // "Investigation complete", no root cause
	if rc := priorRootCause(sql.NullString{String: bare, Valid: true}); rc != "" {
		t.Fatalf("bare done (no root cause) must be dropped, got %q", rc)
	}
}

func TestSelectPriors_HostOverlapThenRecency(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	cands := []store.PriorInvestigation{
		donePrior("p-old-h1", []string{"h1"}, "old h1 cause", now.Add(-2*time.Hour)),
		donePrior("p-new-h1", []string{"h1"}, "new h1 cause", now.Add(-1*time.Hour)),
		donePrior("p-h2", []string{"h2"}, "h2 cause", now.Add(-30*time.Minute)),
		donePrior("p-inconclusive", []string{"h1"}, "inconclusive", now),
	}
	cfg := PriorsConfig{Enabled: true, MaxInvestigations: 4, MaxFindingsPerInv: 3, Scope: "host_overlap", MaxAgeDays: 30}
	got := selectPriors(cands, []string{"h1"}, now, cfg)
	if len(got) != 2 {
		t.Fatalf("want 2 (h1 overlap, inconclusive dropped, h2 excluded), got %d: %+v", len(got), got)
	}
	if got[0].ID != "p-new-h1" || got[1].ID != "p-old-h1" {
		t.Fatalf("want newest-first [p-new-h1, p-old-h1], got [%s, %s]", got[0].ID, got[1].ID)
	}
}

func TestSelectPriors_FleetWideFallsBackToRecency(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	cands := []store.PriorInvestigation{
		donePrior("a", []string{"h1"}, "ca", now.Add(-3*time.Hour)),
		donePrior("b", []string{"h2"}, "cb", now.Add(-1*time.Hour)),
	}
	// Empty newHosts = "all hosts" → no host axis → recency fallback, both kept.
	got := selectPriors(cands, nil, now, DefaultPriorsConfig())
	if len(got) != 2 || got[0].ID != "b" {
		t.Fatalf("fleet-wide should include all by recency (b first), got %+v", got)
	}
}

func TestSelectPriors_AgeFilter(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	cands := []store.PriorInvestigation{
		donePrior("fresh", []string{"h1"}, "c", now.Add(-2*24*time.Hour)),
		donePrior("stale", []string{"h1"}, "c", now.Add(-40*24*time.Hour)),
	}
	got := selectPriors(cands, []string{"h1"}, now, DefaultPriorsConfig()) // MaxAgeDays 30
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("stale prior (>30d) must be excluded, got %+v", got)
	}
}

func TestRenderPriorsDigest_CapsAndEmpty(t *testing.T) {
	if d, rendered := RenderPriorsDigest(nil, DefaultPriorsConfig()); d != "" || rendered != nil {
		t.Fatal("empty input must render empty string and no records")
	}
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	recs := []PriorRecord{{
		ID: "inv1", CreatedAt: now, Hosts: []string{"h1"}, RootCause: strings.Repeat("x", 500),
		Findings: []store.Finding{
			{Severity: "error", Code: "c1", Message: strings.Repeat("y", 300)},
			{Severity: "warn", Code: "c2", Message: "short"},
			{Severity: "info", Code: "c3", Message: "extra"},
		},
	}}
	cfg := PriorsConfig{Enabled: true, MaxInvestigations: 4, MaxFindingsPerInv: 2, Scope: "host_overlap", MaxAgeDays: 30}
	out, _ := RenderPriorsDigest(recs, cfg)
	if !strings.Contains(out, "inv1") || !strings.Contains(out, "hosts: h1") {
		t.Fatalf("digest missing prior header: %q", out)
	}
	if !strings.Contains(out, "…(truncated)") {
		t.Fatalf("over-cap root cause must be truncated: %q", out)
	}
	if n := strings.Count(out, "•"); n != 2 {
		t.Fatalf("only MaxFindingsPerInv (2) findings must render, got %d: %q", n, out)
	}
	if strings.Contains(out, "c3") {
		t.Fatalf("findings beyond the cap must be dropped: %q", out)
	}
}

func TestRenderPriorsDigest_HardTokenCapTruncatesList(t *testing.T) {
	now := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	var recs []PriorRecord
	for i := 0; i < 50; i++ {
		recs = append(recs, PriorRecord{
			ID: fmt.Sprintf("inv%02d", i), CreatedAt: now, Hosts: []string{"h1"},
			RootCause: strings.Repeat("z", 280),
		})
	}
	cfg := PriorsConfig{Enabled: true, MaxInvestigations: 50, MaxFindingsPerInv: 0, Scope: "host_overlap"}
	out, rendered := RenderPriorsDigest(recs, cfg)
	if got := tokensForBytes(len(out)); got > priorsDigestTokenCap {
		t.Fatalf("digest exceeded the hard token cap: %d > %d", got, priorsDigestTokenCap)
	}
	n := strings.Count(out, "- inv")
	if n == 0 || n >= 50 {
		t.Fatalf("hard cap should render some but not all 50 priors, got %d", n)
	}
	if len(rendered) != n {
		t.Fatalf("rendered records (%d) must match prior lines in digest (%d)", len(rendered), n)
	}
}

// Non-done priors (operator-attached aborted/active) render with a status tag
// and fall back to their findings, or a note when they have neither.
func TestRenderPriorsDigest_NonDoneStatusTag(t *testing.T) {
	recs := []PriorRecord{
		{ID: "ab1", Status: "aborted", Hosts: []string{"h"}, Findings: []store.Finding{{Severity: "error", Code: "c1", Message: "boom"}}},
		{ID: "ac1", Status: "active"}, // no conclusion, no findings
	}
	out, rendered := RenderPriorsDigest(recs, PriorsConfig{Enabled: true, MaxInvestigations: 4, MaxFindingsPerInv: 3})
	if len(rendered) != 2 {
		t.Fatalf("both non-done priors must render, got %d", len(rendered))
	}
	if !strings.Contains(out, "[aborted]") || !strings.Contains(out, "(see findings)") || !strings.Contains(out, "boom") {
		t.Fatalf("aborted-with-findings render wrong: %q", out)
	}
	if !strings.Contains(out, "[active]") || !strings.Contains(out, "(no findings or conclusion recorded)") {
		t.Fatalf("empty active prior render wrong: %q", out)
	}
}
