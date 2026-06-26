package investigator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// priorsSeedHeader prefixes the injected digest. The model is told these are
// HINTS from earlier investigations, not established facts for THIS incident —
// it must re-verify, and the evidence-first invariant (handleAddFinding) already
// blocks any prior conclusion from becoming finding evidence on its own.
const priorsSeedHeader = "## Prior investigations (read-only context) [CROSS_INVESTIGATION_HINT]\n" +
	"Everything under this [CROSS_INVESTIGATION_HINT] heading is from OTHER, earlier investigations — never confuse it " +
	"with evidence collected in THIS run. Each entry below is the INCIDENT and the CONCLUSION of a SEPARATE, earlier " +
	"investigation on an overlapping host. They are HINTS about the fleet, NOT facts about THIS incident.\n" +
	"MUST: do NOT adopt a prior's conclusion as this incident's root cause without independently re-deriving it from " +
	"THIS investigation's own evidence and symptoms. First compare each prior's incident to THIS incident's PRIMARY " +
	"symptom (rule 15): if they differ, treat the prior as LOW relevance and do not let it bias your differential — a " +
	"prior that blamed subsystem X does not make X the cause here. Cite only THIS investigation's task_ids in add_finding.\n" +
	"RETRIEVAL: each entry below is a one-line DIGEST, not the full prior. To pull a prior's untruncated conclusion, " +
	"recommended remediation and findings — e.g. when the operator says 'the config from before' / 'again' / 'as we " +
	"found last time' — call recall_prior(investigation_id=<the inv_… id listed below>) INSTEAD of re-collecting from the host.\n\n"

// buildPriorsSeed assembles the cross-investigation priors digest for a new
// investigation, or returns "" when disabled / nothing relevant. Best-effort:
// any store error yields "" (priors are an aid, never a hard dependency, so they
// must never block or fail Start).
// buildPriorsSeed assembles the cross-investigation priors digest for a new
// investigation and returns (seed, attachedIDs). attachedIDs is exactly the set
// of prior investigations whose conclusions made it into the digest (after the
// hard token cap), so the caller can record them for operator visibility.
// Returns ("", nil) when disabled or nothing relevant. manualPriorIDs are
// operator-selected priors, honored regardless of host overlap and merged with
// the automatic host-scoped selection. Best-effort: store errors degrade to a
// smaller/empty digest, never a hard failure of Start.
func (l *Loop) buildPriorsSeed(ctx context.Context, newInvID string, allowedHosts, manualPriorIDs []string) (string, []string) {
	cfg := l.priorsConfig
	if !cfg.Enabled {
		return "", nil
	}
	// Manual (operator-selected) priors: honored regardless of host overlap —
	// the operator chose them explicitly. Skipped only if they carry no usable
	// conclusion.
	var manual []PriorRecord
	manualSet := map[string]bool{}
	if len(manualPriorIDs) > 0 {
		picked, err := l.store.ListInvestigationsByIDs(ctx, manualPriorIDs)
		if err != nil && l.log != nil {
			l.log.Debug("priors: list manual priors failed", "investigation_id", newInvID, "err", err)
		}
		for _, c := range picked {
			// Operator chose it explicitly — include regardless of status. For a
			// non-done pick priorRootCause is "" and the digest falls back to its
			// findings (attached in the merge loop below) or a status note.
			manual = append(manual, PriorRecord{ID: c.ID, Goal: c.Goal, Status: c.Status, CreatedAt: c.CreatedAt, Hosts: c.AllowedHosts, RootCause: priorRootCause(c.SummaryJSON)})
			manualSet[c.ID] = true
		}
	}
	// Auto (host-overlap) priors, excluding anything already picked manually.
	fetch := cfg.MaxInvestigations * 4
	if fetch < 8 {
		fetch = 8
	}
	cands, err := l.store.ListRecentDoneInvestigations(ctx, newInvID, fetch)
	if err != nil && l.log != nil {
		l.log.Debug("priors: list done investigations failed", "investigation_id", newInvID, "err", err)
	}
	auto := selectPriors(cands, allowedHosts, time.Now().UTC(), cfg)

	// Merge: manual first (always kept), then auto not already chosen. Cap so an
	// explicit operator pick is never dropped by the auto cap.
	merged := make([]PriorRecord, 0, len(manual)+len(auto))
	merged = append(merged, manual...)
	for _, r := range auto {
		if manualSet[r.ID] {
			continue
		}
		merged = append(merged, r)
	}
	limit := cfg.MaxInvestigations
	if len(manual) > limit {
		limit = len(manual)
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	if len(merged) == 0 {
		if l.log != nil {
			l.log.Debug("priors: nothing to inject", "investigation_id", newInvID, "manual", len(manual), "auto", len(auto))
		}
		return "", nil
	}
	if cfg.MaxFindingsPerInv > 0 {
		for i := range merged {
			fs, ferr := l.store.ListFindings(ctx, merged[i].ID)
			if ferr != nil {
				continue
			}
			merged[i].Findings = topFindings(fs, cfg.MaxFindingsPerInv)
		}
	}
	digest, rendered := RenderPriorsDigest(merged, cfg)
	if digest == "" {
		return "", nil
	}
	ids := make([]string, 0, len(rendered))
	for _, r := range rendered {
		ids = append(ids, r.ID)
	}
	return priorsSeedHeader + digest, ids
}

// topFindings returns up to n findings, preferring pinned and higher severity
// and dropping ignored ones — the most useful priors to surface in the digest.
// findingsDigestCap bounds how many findings the post-compaction recall digest
// lists (highest-severity / pinned first via topFindings).
const findingsDigestCap = 20

// findingEvidenceRefs extracts the evidence task_ids from a finding's
// EvidenceJSON (stored as {"task_ids":[...]} by handleAddFinding).
func findingEvidenceRefs(evidenceJSON string) []string {
	var e struct {
		TaskIDs []string `json:"task_ids"`
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &e); err != nil {
		return nil
	}
	return e.TaskIDs
}

// buildFindingsDigest renders THIS investigation's active findings (ignored
// dropped, highest-severity/pinned first) as a compact recall block that
// compact() re-injects after folding older messages. It is the deterministic
// floor for post-compaction recall: finding_ids and their evidence task_ids
// survive the archive verbatim, independent of what the compaction summary
// happened to carry. Returns "" when there are no active findings.
func buildFindingsDigest(findings []store.Finding) string {
	top := topFindings(findings, findingsDigestCap)
	if len(top) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("FINDINGS SO FAR (this investigation — recall after compaction). " +
		"Cite the evidence task_ids in add_finding (finding_id is a reference handle, not an evidence_ref):\n")
	for _, f := range top {
		fmt.Fprintf(&b, "- [%s] %s: %s (finding_id=%s", f.Severity, f.Code, truncateRunes(f.Message, 200), f.ID)
		if f.Pinned {
			b.WriteString("; pinned")
		}
		if refs := findingEvidenceRefs(f.EvidenceJSON); len(refs) > 0 {
			b.WriteString("; evidence task_ids: " + strings.Join(refs, ", "))
		}
		b.WriteString(")\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// buildIgnoredBranchesDigest renders the operator-IGNORED findings of THIS
// investigation as a compact recall block that compact() re-injects after
// folding older messages. buildFindingsDigest deliberately DROPS ignored
// findings (they are not evidence to cite); this is their deterministic
// survival floor, so the rule-5 "do not re-enter an IGNORED branch" invariant
// outlives compaction independent of what the summary LLM happened to carry.
// Returns "" when there are no ignored findings.
func buildIgnoredBranchesDigest(findings []store.Finding) string {
	var ignored []store.Finding
	for _, f := range findings {
		if f.Ignored {
			ignored = append(ignored, f)
		}
	}
	if len(ignored) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("IGNORED branches (operator closed these — do NOT re-enter, rule 5):\n")
	for _, f := range ignored {
		fmt.Fprintf(&b, "- [%s] %s: %s (finding_id=%s)\n",
			f.Severity, f.Code, truncateRunes(f.Message, 200), f.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}

func topFindings(fs []store.Finding, n int) []store.Finding {
	sev := func(s string) int {
		switch s {
		case "critical":
			return 3
		case "error":
			return 2
		case "warn":
			return 1
		}
		return 0
	}
	filtered := make([]store.Finding, 0, len(fs))
	for _, f := range fs {
		if f.Ignored {
			continue
		}
		filtered = append(filtered, f)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Pinned != filtered[j].Pinned {
			return filtered[i].Pinned
		}
		return sev(filtered[i].Severity) > sev(filtered[j].Severity)
	})
	if n >= 0 && len(filtered) > n {
		filtered = filtered[:n]
	}
	return filtered
}

// PriorsConfig controls the cross-investigation priors digest injected into a
// new investigation's context. All values are operator-configurable
// (investigator.priors.* in hub.yaml / env); see cmd/hub/config.go.
type PriorsConfig struct {
	Enabled           bool
	MaxInvestigations int    // cap N priors in the digest
	MaxFindingsPerInv int    // cap M findings shown per prior
	Scope             string // "host_overlap" (default) | "recent_any"
	MaxAgeDays        int    // skip priors older than this (0 = no age cap)
}

// DefaultPriorsConfig is the compiled-in default; cmd/hub overrides it from
// hub.yaml / env (CLAUDE.md: settings external, never hardcoded at the call).
func DefaultPriorsConfig() PriorsConfig {
	return PriorsConfig{Enabled: true, MaxInvestigations: 4, MaxFindingsPerInv: 3, Scope: "host_overlap", MaxAgeDays: 30}
}

const (
	priorGoalCap         = 200  // per-prior incident/goal char cap (relevance check)
	priorRootCauseCap    = 280  // per-prior root-cause char cap
	priorFindingMsgCap   = 140  // per-finding message char cap
	priorsDigestTokenCap = 1500 // HARD total cap on the whole digest (three-tier invariant)
)

// PriorRecord is a selected prior investigation with its parsed conclusion and
// (optionally) top findings, ready for RenderPriorsDigest.
type PriorRecord struct {
	ID        string
	Goal      string // the prior's incident/symptom — surfaced so the model can judge relevance
	Status    string
	CreatedAt time.Time
	Hosts     []string
	RootCause string
	Findings  []store.Finding
}

// priorRootCause extracts a usable one-line conclusion from a done
// investigation's terminal summary, or "" when there is none worth sharing —
// a missing/legacy payload, a bare "Investigation complete" with no root cause,
// or an explicitly "inconclusive" close (injecting that would be noise). Uses
// ParseInvestigationTerminalPayload so legacy payloads are tolerated.
func priorRootCause(summary sql.NullString) string {
	p, ok := store.ParseInvestigationTerminalPayload(summary)
	if !ok {
		return ""
	}
	const donePrefix = "Investigation complete:"
	reason := oneLine(p.Reason)
	if !strings.HasPrefix(reason, donePrefix) {
		return "" // bare "Investigation complete" / non-done reason → nothing useful
	}
	rc := strings.TrimSpace(strings.TrimPrefix(reason, donePrefix))
	if rc == "" || strings.EqualFold(rc, "inconclusive") {
		return ""
	}
	return rc
}

// selectPriors ranks and filters candidate done-investigations into the priors
// to inject. With host scope (the default) it keeps ONLY priors that share at
// least one host with the new investigation — the operator asked for "same
// host/target" — ordered by overlap count then recency. It falls back to
// recency-only when scope is "recent_any" or the new investigation is fleet-wide
// (empty newHosts, i.e. "all hosts"), so a fleet investigation still gets
// recent priors instead of nothing. Priors with no usable conclusion or older
// than MaxAgeDays are dropped. Returns at most cfg.MaxInvestigations records.
func selectPriors(cands []store.PriorInvestigation, newHosts []string, now time.Time, cfg PriorsConfig) []PriorRecord {
	hostSet := make(map[string]bool, len(newHosts))
	for _, h := range newHosts {
		hostSet[h] = true
	}
	// "all hosts" (empty scope list) has no host axis to intersect, so host
	// overlap is meaningless → fall back to recency.
	useHostOverlap := cfg.Scope != "recent_any" && len(hostSet) > 0

	type scored struct {
		rec     PriorRecord
		overlap int
	}
	var list []scored
	for _, c := range cands {
		if cfg.MaxAgeDays > 0 && !c.CreatedAt.IsZero() &&
			now.Sub(c.CreatedAt) > time.Duration(cfg.MaxAgeDays)*24*time.Hour {
			continue
		}
		rc := priorRootCause(c.SummaryJSON)
		if rc == "" {
			continue
		}
		overlap := 0
		for _, h := range c.AllowedHosts {
			if hostSet[h] {
				overlap++
			}
		}
		if useHostOverlap && overlap == 0 {
			continue // "same host" scope: an unrelated prior is noise, not a hint
		}
		list = append(list, scored{
			rec:     PriorRecord{ID: c.ID, Goal: c.Goal, Status: c.Status, CreatedAt: c.CreatedAt, Hosts: c.AllowedHosts, RootCause: rc},
			overlap: overlap,
		})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if useHostOverlap && list[i].overlap != list[j].overlap {
			return list[i].overlap > list[j].overlap
		}
		return list[i].rec.CreatedAt.After(list[j].rec.CreatedAt)
	})
	max := cfg.MaxInvestigations
	if max <= 0 {
		max = 4
	}
	out := make([]PriorRecord, 0, max)
	for _, s := range list {
		if len(out) >= max {
			break
		}
		out = append(out, s.rec)
	}
	return out
}

// RenderPriorsDigest renders selected priors into a compact, bounded Markdown
// block for injection into a new investigation's context. Per-item char caps
// PLUS a hard total token cap (priorsDigestTokenCap) keep it within the
// three-tier context budget — it is a re-grounding hint, never a data dump.
// Returns ("", nil) when there is nothing to show. The second return value is
// the subset of records actually rendered (the token cap may truncate the
// list), so callers can record exactly which priors were injected.
func RenderPriorsDigest(recs []PriorRecord, cfg PriorsConfig) (string, []PriorRecord) {
	if len(recs) == 0 {
		return "", nil
	}
	maxFindings := cfg.MaxFindingsPerInv
	if maxFindings < 0 {
		maxFindings = 0
	}
	var b strings.Builder
	var rendered []PriorRecord
	for _, r := range recs {
		hosts := "all hosts"
		if len(r.Hosts) > 0 {
			hosts = strings.Join(r.Hosts, ", ")
		}
		date := ""
		if !r.CreatedAt.IsZero() {
			date = r.CreatedAt.UTC().Format("2006-01-02")
		}
		// Non-done priors (operator-attached aborted/active runs) carry a status
		// tag and fall back to their findings, since they have no final conclusion.
		statusTag := ""
		if r.Status != "" && r.Status != "done" {
			statusTag = " [" + r.Status + "]"
		}
		concl := oneLine(capNotebook(r.RootCause, priorRootCauseCap))
		if concl == "" {
			if len(r.Findings) > 0 {
				concl = "(see findings)"
			} else {
				concl = "(no findings or conclusion recorded)"
			}
		}
		var item strings.Builder
		fmt.Fprintf(&item, "- %s (%s, hosts: %s)%s\n", r.ID, date, hosts, statusTag)
		// Surface the prior's INCIDENT (its goal/symptom) separately from its
		// conclusion so the model can judge relevance to THIS incident (rule 15)
		// instead of anchoring on a possibly-unrelated prior conclusion.
		if g := oneLine(capNotebook(r.Goal, priorGoalCap)); g != "" {
			fmt.Fprintf(&item, "    its incident: %s\n", g)
		}
		fmt.Fprintf(&item, "    its conclusion: %s\n", concl)
		for i, f := range r.Findings {
			if i >= maxFindings {
				break
			}
			fmt.Fprintf(&item, "  • [%s] %s — %s\n",
				strings.ToUpper(f.Severity), f.Code, oneLine(capNotebook(f.Message, priorFindingMsgCap)))
		}
		// HARD total cap: stop adding priors once the digest would exceed the
		// token budget. Truncates the LIST, not just each item, so the injected
		// block can never bloat the LLM context.
		if tokensForBytes(b.Len()+item.Len()) > priorsDigestTokenCap {
			break
		}
		b.WriteString(item.String())
		rendered = append(rendered, r)
	}
	return b.String(), rendered
}
