package investigator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

const (
	// investigationsSubdir is the artifact-root subtree that holds
	// investigation-scoped artifacts (notebooks, future exports). It is kept
	// distinct from the per-task <artifact_dir>/<task_id> layout so retention
	// (Task 7A) can apply an investigation-scoped policy instead of treating
	// the directory as an orphan task id.
	investigationsSubdir = "investigations"
	notebookFileName     = "notebook.md"

	// notebookFieldCap bounds a single curated value written to the notebook.
	// The notebook links task/finding/memory IDs and keeps only short
	// excerpts — it must never become a dump of raw collector output.
	notebookFieldCap   = 4096
	notebookSummaryCap = 8192
)

// InvestigationArtifactDir resolves the directory that holds an
// investigation's own artifacts: <root>/investigations/<investigation_id>.
// It guarantees the result stays within <root>/investigations and never
// escapes via "..", absolute ids, or path separators in the id. The
// directory is NOT created — callers that write do their own MkdirAll.
func InvestigationArtifactDir(root, investigationID string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("artifact root not configured")
	}
	id := strings.TrimSpace(investigationID)
	if id == "" {
		return "", fmt.Errorf("investigation id required")
	}
	// Investigation ids are opaque tokens (inv_<hex>). Reject anything that
	// could traverse out of the investigations subtree.
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", fmt.Errorf("invalid investigation id %q", investigationID)
	}
	base := filepath.Join(filepath.Clean(root), investigationsSubdir)
	dir := filepath.Join(base, id)
	// id must be a single, non-traversing path element: <base>/<id>.
	if filepath.Clean(dir) != dir || filepath.Dir(dir) != base {
		return "", fmt.Errorf("invalid investigation id %q", investigationID)
	}
	return dir, nil
}

// NotebookPath returns the absolute notebook.md path for an investigation,
// validated to stay within the artifact root.
func NotebookPath(root, investigationID string) (string, error) {
	dir, err := InvestigationArtifactDir(root, investigationID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, notebookFileName), nil
}

// Notebook appends a curated, human-readable Markdown record of an
// investigation's key events (findings, terminal summaries, operator
// hypotheses, compaction memory). It is best-effort: every method logs and
// returns on failure but never blocks the investigation loop. A nil or
// unconfigured (empty root) Notebook is a no-op.
type Notebook struct {
	root string
	log  *slog.Logger
}

// NewNotebook builds a Notebook rooted at the artifact directory. An empty
// root disables it (every method becomes a no-op).
func NewNotebook(root string, log *slog.Logger) *Notebook {
	return &Notebook{root: strings.TrimSpace(root), log: log}
}

func (n *Notebook) enabled() bool { return n != nil && n.root != "" }

func (n *Notebook) warn(investigationID, op string, err error) {
	if n != nil && n.log != nil {
		n.log.Warn("notebook write failed",
			"investigation_id", investigationID, "op", op, "err", err)
	}
}

// Path exposes the resolved notebook path for UI/API link rendering.
func (n *Notebook) Path(investigationID string) (string, bool) {
	if !n.enabled() {
		return "", false
	}
	p, err := NotebookPath(n.root, investigationID)
	if err != nil {
		return "", false
	}
	if _, statErr := os.Stat(p); statErr != nil {
		return p, false
	}
	return p, true
}

// Read returns the raw notebook bytes for export/download, bounded to a sane
// cap so a runaway file can't be slurped whole into an HTTP response.
func (n *Notebook) Read(investigationID string, maxBytes int64) ([]byte, error) {
	if !n.enabled() {
		return nil, fmt.Errorf("notebook disabled")
	}
	p, err := NotebookPath(n.root, investigationID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // path validated by NotebookPath
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && int64(len(b)) > maxBytes {
		b = append(b[:maxBytes], []byte("\n…(truncated)\n")...)
	}
	return b, nil
}

// Create writes the notebook header once at investigation start. If a
// notebook already exists (e.g. the hub restarted and the investigation
// resumed), it is left untouched.
func (n *Notebook) Create(inv store.Investigation, contextWindowTokens, maxOutputTokens int, startedAt time.Time) error {
	if !n.enabled() {
		return nil
	}
	dir, err := InvestigationArtifactDir(n.root, inv.ID)
	if err != nil {
		n.warn(inv.ID, "resolve", err)
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		n.warn(inv.ID, "mkdir", err)
		return err
	}
	path := filepath.Join(dir, notebookFileName)
	if _, statErr := os.Stat(path); statErr == nil {
		return nil // do not clobber an existing notebook on resume
	}
	allowed := "all hosts"
	if len(inv.AllowedHosts) > 0 {
		allowed = strings.Join(inv.AllowedHosts, ", ")
	}
	header := fmt.Sprintf("# Investigation %s\n\n", inv.ID) +
		nbFields(
			[2]string{"Goal", inv.Goal},
			[2]string{"Model", inv.Model},
			[2]string{"Context window (tokens)", fmt.Sprintf("%d", contextWindowTokens)},
			[2]string{"Max output (tokens)", fmt.Sprintf("%d", maxOutputTokens)},
			[2]string{"Allowed hosts", allowed},
			[2]string{"Started", startedAt.UTC().Format(time.RFC3339)},
			[2]string{"Created by", inv.CreatedBy},
		)
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		n.warn(inv.ID, "create", err)
		return err
	}
	if n.log != nil {
		n.log.Info("notebook created", "investigation_id", inv.ID, "path", path)
	}
	return nil
}

// AppendFinding records a finding with stable anchors for the finding and its
// durable memory record. evidenceRefs are the cited task_ids; memoryID may be
// empty if the durable memory write failed.
func (n *Notebook) AppendFinding(investigationID string, f store.Finding, evidenceRefs []string, memoryID string) error {
	body := nbFields(
		[2]string{"Time", time.Now().UTC().Format(time.RFC3339)},
		[2]string{"Finding", f.ID},
		[2]string{"Severity", f.Severity},
		[2]string{"Code", f.Code},
		[2]string{"Message", f.Message},
		[2]string{"Evidence (task_ids)", strings.Join(evidenceRefs, ", ")},
		[2]string{"Memory", memoryID},
	)
	title := fmt.Sprintf("Finding %s [%s]", f.ID, f.Severity)
	return n.append(investigationID, nbSection("finding-"+f.ID, title, body))
}

// AppendMarkDone records the final post-mortem summary. The raw summary JSON
// is embedded in a bounded fenced block; root_cause is surfaced as a field.
//
// Latest-only dedup: across a reopen→reclose the model can call mark_done
// again, and each accepted close used to append another "## Conclusion
// (mark_done)" section (inv_a00000000005 stacked 11). summary_json already
// keeps only the latest conclusion (FinishInvestigation overwrites it) and the
// web Conclusion card reads from it, so the notebook only needs the conclusion
// section ONCE. We suppress the repeat instead of stacking, and never rewrite
// notebook.md in place (operator decision) — the first close's section stays.
func (n *Notebook) AppendMarkDone(investigationID, summaryJSON string) error {
	if n.hasConclusion(investigationID) {
		if n != nil && n.log != nil {
			n.log.Debug("notebook conclusion already present; skipping duplicate mark_done append",
				"investigation_id", investigationID)
		}
		return nil
	}
	rootCause := ""
	var s struct {
		RootCause  string `json:"root_cause"`
		Confidence string `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(summaryJSON), &s); err == nil {
		rootCause = s.RootCause
	}
	body := nbFields(
		[2]string{"Time", time.Now().UTC().Format(time.RFC3339)},
		[2]string{"Confidence", s.Confidence},
		[2]string{"Root cause", rootCause},
	)
	body += "\n```json\n" + capNotebook(summaryJSON, notebookSummaryCap) + "\n```\n"
	return n.append(investigationID, nbSection("done", "Conclusion (mark_done)", body))
}

// AppendAbort records why an investigation terminated abnormally.
func (n *Notebook) AppendAbort(investigationID string, payload store.InvestigationTerminalPayload) error {
	body := nbFields(
		[2]string{"Time", time.Now().UTC().Format(time.RFC3339)},
		[2]string{"Kind", payload.Kind},
		[2]string{"Reason", payload.Reason},
		[2]string{"Source", payload.Source},
		[2]string{"Recoverable", fmt.Sprintf("%t", payload.Recoverable)},
		[2]string{"Detail", capNotebook(payload.Detail, notebookFieldCap)},
	)
	return n.append(investigationID, nbSection("aborted", "Aborted", body))
}

// AppendOperatorHypothesis records an operator hypothesis that redirected the
// investigation (PROJECT.md §7.5).
func (n *Notebook) AppendOperatorHypothesis(investigationID, claim, expected, instruction string) error {
	body := nbFields(
		[2]string{"Time", time.Now().UTC().Format(time.RFC3339)},
		[2]string{"Claim", claim},
		[2]string{"Expected evidence", expected},
		[2]string{"Instruction", instruction},
	)
	return n.append(investigationID, nbSection("", "Operator hypothesis", body))
}

// AppendMemory records a durable memory write (e.g. a compaction summary) so
// the operator can see what was retained when older messages were folded.
func (n *Notebook) AppendMemory(investigationID string, m store.InvestigationMemory) error {
	body := nbFields(
		[2]string{"Time", time.Now().UTC().Format(time.RFC3339)},
		[2]string{"Memory", m.ID},
		[2]string{"Kind", m.Kind},
		[2]string{"Message seq range", fmt.Sprintf("%d–%d", m.MessageSeqStart, m.MessageSeqEnd)},
		[2]string{"Token estimate", fmt.Sprintf("%d", m.TokenEstimate)},
	)
	body += "\n" + capNotebook(m.Content, notebookSummaryCap) + "\n"
	return n.append(investigationID, nbSection("memory-"+m.ID, "Memory "+m.ID+" ("+m.Kind+")", body))
}

// hasConclusion reports whether a "Conclusion (mark_done)" section was already
// written for this investigation (matched on the stable anchor the section
// renders with). Used to dedup the conclusion across a reopen→reclose cycle
// without rewriting notebook.md.
func (n *Notebook) hasConclusion(investigationID string) bool {
	if !n.enabled() {
		return false
	}
	dir, err := InvestigationArtifactDir(n.root, investigationID)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, notebookFileName))
	if err != nil {
		return false // no notebook yet → no conclusion
	}
	return strings.Contains(string(data), `<a id="done"></a>`)
}

// append is the shared write path: resolve, mkdir, open-append, write.
func (n *Notebook) append(investigationID, section string) error {
	if !n.enabled() {
		return nil
	}
	dir, err := InvestigationArtifactDir(n.root, investigationID)
	if err != nil {
		n.warn(investigationID, "resolve", err)
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		n.warn(investigationID, "mkdir", err)
		return err
	}
	path := filepath.Join(dir, notebookFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path validated above
	if err != nil {
		n.warn(investigationID, "open", err)
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(section); err != nil {
		n.warn(investigationID, "write", err)
		return err
	}
	return nil
}

// nbSection renders one Markdown section with an optional stable HTML anchor.
func nbSection(anchor, title, body string) string {
	var b strings.Builder
	b.WriteString("\n")
	if anchor != "" {
		fmt.Fprintf(&b, "<a id=%q></a>\n", anchor)
	}
	b.WriteString("## ")
	b.WriteString(oneLine(title))
	b.WriteString("\n\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// nbFields renders a bullet list of label/value pairs, skipping empty values.
func nbFields(pairs ...[2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		if strings.TrimSpace(p[1]) == "" {
			continue
		}
		fmt.Fprintf(&b, "- **%s:** %s\n", p[0], oneLine(capNotebook(p[1], notebookFieldCap)))
	}
	return b.String()
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func capNotebook(s string, maxBytes int) string {
	if len(s) > maxBytes {
		return s[:maxBytes] + "…(truncated)"
	}
	return s
}
