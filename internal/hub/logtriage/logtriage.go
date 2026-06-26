// Package logtriage provides deterministic, CPU-only log analysis for the
// hub: compact per-artifact indexes (Task 10) and severity/template-clustered
// triage (Task 11).
//
// It is intentionally CPU-first and dependency-free (Task 10A): no embeddings,
// rerankers, GPU, or CUDA — pure stdlib so default deployments work without
// extra drivers. It is also a leaf package (imports only the standard library)
// so both internal/hub/runner and internal/hub/investigator can use it without
// creating an import cycle (investigator already imports runner).
package logtriage

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// ScanCap bounds how much of an artifact we read for indexing/triage. The
	// index is a navigation aid, never a full load; larger files are scanned
	// over their prefix and flagged truncated.
	ScanCap = 4 << 20 // 4 MiB

	maxClusters    = 12
	maxExcerptLine = 400
	firstLastN     = 5
	burstThreshold = 5 // a cluster with >= this many lines is a "burst"

	// IndexFileName is the on-disk name of the per-task artifact index written
	// beside the artifacts under <artifact_dir>/<task_id>/.
	IndexFileName = "_index.json"
)

// Cluster is a group of log lines that share a normalized template.
type Cluster struct {
	Template  string `json:"template"`
	Count     int    `json:"count"`
	Severity  string `json:"severity,omitempty"`
	Unit      string `json:"unit,omitempty"`
	FirstLine int    `json:"first_line"`
	LastLine  int    `json:"last_line"`
	Example   string `json:"example"`
}

// ArtifactIndex is the compact, LLM-facing summary of one artifact file.
type ArtifactIndex struct {
	Name        string    `json:"name"`
	SizeBytes   int64     `json:"size_bytes"`
	LineCount   int       `json:"line_count,omitempty"`
	Binary      bool      `json:"binary,omitempty"`
	Truncated   bool      `json:"truncated,omitempty"`
	TimeStart   string    `json:"time_start,omitempty"`
	TimeEnd     string    `json:"time_end,omitempty"`
	Units       []string  `json:"units,omitempty"`
	TopPatterns []Cluster `json:"top_patterns,omitempty"`
	FirstLines  []string  `json:"first_lines,omitempty"`
	LastLines   []string  `json:"last_lines,omitempty"`
}

// HostClusters carries one host's log clusters for cross-host roll-up. The
// investigator builds one per task in a collect_batch before calling
// RollupClusters.
type HostClusters struct {
	HostID   string
	TaskID   string
	Clusters []Cluster
}

// HostClusterRef is one host's contribution to a rolled-up cluster. HostID +
// TaskID + line range let the investigator drill into that specific host via
// search_artifact / get_full_result without re-loading every host's patterns.
type HostClusterRef struct {
	HostID    string `json:"host_id"`
	TaskID    string `json:"task_id,omitempty"`
	Count     int    `json:"count"`
	FirstLine int    `json:"first_line,omitempty"`
	LastLine  int    `json:"last_line,omitempty"`
}

// RolledCluster is the same normalized (Template, Severity) seen across one or
// more hosts, with the total count and a bounded per-host breakdown. It
// replaces N repeated per-host TopPatterns blocks with a single cross-host
// entry in collect_batch summaries.
type RolledCluster struct {
	Template     string           `json:"template"`
	Severity     string           `json:"severity,omitempty"`
	Unit         string           `json:"unit,omitempty"`
	TotalCount   int              `json:"total_count"`
	HostCount    int              `json:"host_count"`
	Example      string           `json:"example,omitempty"`
	PerHost      []HostClusterRef `json:"per_host,omitempty"`
	OmittedHosts int              `json:"omitted_hosts,omitempty"`
}

// RollupClusters merges per-host clusters by (Template, Severity) into a
// severity/count-ordered list of cross-host clusters. It is deterministic: the
// output order and per-host breakdown never depend on map iteration order.
// maxHostsPerCluster bounds the per-cluster host list (overflow counted in
// OmittedHosts); the result is capped at maxClusters after ranking.
func RollupClusters(perHost []HostClusters, maxHostsPerCluster int) []RolledCluster {
	if maxHostsPerCluster <= 0 {
		maxHostsPerCluster = maxClusters
	}
	type agg struct {
		rc   RolledCluster
		refs []HostClusterRef
	}
	order := make([]string, 0)
	byKey := map[string]*agg{}
	for _, hc := range perHost {
		for _, c := range hc.Clusters {
			key := c.Severity + "\x00" + c.Template
			a, ok := byKey[key]
			if !ok {
				a = &agg{rc: RolledCluster{
					Template: c.Template, Severity: c.Severity, Unit: c.Unit, Example: c.Example,
				}}
				byKey[key] = a
				order = append(order, key)
			}
			if a.rc.Unit == "" && c.Unit != "" {
				a.rc.Unit = c.Unit
			}
			if a.rc.Example == "" && c.Example != "" {
				a.rc.Example = c.Example
			}
			a.rc.TotalCount += c.Count
			a.refs = append(a.refs, HostClusterRef{
				HostID: hc.HostID, TaskID: hc.TaskID, Count: c.Count,
				FirstLine: c.FirstLine, LastLine: c.LastLine,
			})
		}
	}
	out := make([]RolledCluster, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		refs := a.refs
		// Deterministic per-host order: count desc, then host_id asc.
		sort.SliceStable(refs, func(i, j int) bool {
			if refs[i].Count != refs[j].Count {
				return refs[i].Count > refs[j].Count
			}
			return refs[i].HostID < refs[j].HostID
		})
		a.rc.HostCount = len(refs)
		if len(refs) > maxHostsPerCluster {
			a.rc.OmittedHosts = len(refs) - maxHostsPerCluster
			refs = refs[:maxHostsPerCluster]
		}
		a.rc.PerHost = refs
		out = append(out, a.rc)
	}
	// Rank: critical/error first, then by total count, then template for a
	// stable order independent of host arrival order.
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := severityRank(out[i].Severity), severityRank(out[j].Severity)
		if si != sj {
			return si > sj
		}
		if out[i].TotalCount != out[j].TotalCount {
			return out[i].TotalCount > out[j].TotalCount
		}
		return out[i].Template < out[j].Template
	})
	if len(out) > maxClusters {
		out = out[:maxClusters]
	}
	return out
}

// Triage is the structured result of clustering a log artifact: it replaces
// arbitrary raw chunks in SummarizeTasks with severity/template clusters and
// line references.
type Triage struct {
	Lines         int       `json:"lines"`
	Parsed        int       `json:"parsed"`
	Bursts        int       `json:"bursts,omitempty"`
	OmittedLines  int       `json:"omitted_lines,omitempty"`
	ParseFallback bool      `json:"parse_fallback,omitempty"`
	Truncated     bool      `json:"truncated,omitempty"`
	TimeStart     string    `json:"time_start,omitempty"`
	TimeEnd       string    `json:"time_end,omitempty"`
	Units         []string  `json:"units,omitempty"`
	Clusters      []Cluster `json:"clusters"`
}

var (
	reISOTS    = regexp.MustCompile(`^\s*\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	reSyslogTS = regexp.MustCompile(`^[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}`)
	reUUID     = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reIP       = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	reHex      = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	reHexBlob  = regexp.MustCompile(`\b[0-9a-f]{12,}\b`)
	rePath     = regexp.MustCompile(`(?:/[\w.\-]+){2,}/?`)
	reQuoted   = regexp.MustCompile(`"[^"]*"`)
	reNum      = regexp.MustCompile(`\b\d+\b`)
	reSeverity = regexp.MustCompile(`(?i)\b(emerg|alert|crit|critical|fatal|error|err|warning|warn|notice|info|debug|trace)\b`)
	reUnit     = regexp.MustCompile(`(?:^|\s)([\w.\-]+(?:\.service|\.timer|\.socket|\.scope|\.mount|\.target))\b`)
	reUnitPID  = regexp.MustCompile(`\b([\w.\-]+)\[\d+\]:`)
)

// IndexFile reads an artifact (bounded by ScanCap) and returns its index.
func IndexFile(path string) (ArtifactIndex, error) {
	st, err := os.Stat(path)
	if err != nil {
		return ArtifactIndex{}, err
	}
	f, err := os.Open(path) //nolint:gosec // caller passes a validated artifact path
	if err != nil {
		return ArtifactIndex{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, ScanCap+1))
	if err != nil {
		return ArtifactIndex{}, err
	}
	return IndexBytes(filepath.Base(path), data, st.Size()), nil
}

// IndexBytes builds an index from already-read bytes. fullSize is the true
// on-disk size (data may be a truncated prefix).
func IndexBytes(name string, data []byte, fullSize int64) ArtifactIndex {
	idx := ArtifactIndex{Name: name, SizeBytes: fullSize}
	if isBinary(data) {
		idx.Binary = true
		return idx
	}
	idx.Truncated = int64(len(data)) >= ScanCap && fullSize > int64(len(data))
	tr := triageBytes(data)
	idx.LineCount = tr.Lines
	idx.TimeStart, idx.TimeEnd = tr.TimeStart, tr.TimeEnd
	idx.Units = tr.Units
	idx.TopPatterns = tr.Clusters
	idx.FirstLines, idx.LastLines = headTail(data)
	return idx
}

// TriageFile reads an artifact (bounded) and returns its triage.
func TriageFile(path string) (Triage, error) {
	f, err := os.Open(path) //nolint:gosec // caller passes a validated artifact path
	if err != nil {
		return Triage{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, ScanCap+1))
	if err != nil {
		return Triage{}, err
	}
	tr := triageBytes(data)
	tr.Truncated = int64(len(data)) > ScanCap
	return tr, nil
}

func triageBytes(data []byte) Triage {
	if len(data) > ScanCap {
		data = data[:ScanCap]
	}
	order := []string{}
	byTmpl := map[string]*Cluster{}
	units := map[string]struct{}{}
	tr := Triage{}
	var timeStart, timeEnd string

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		if strings.TrimSpace(raw) == "" {
			continue
		}
		ts, sev, unit, tmpl := parseLine(raw)
		if ts != "" {
			if timeStart == "" {
				timeStart = ts
			}
			timeEnd = ts
		}
		if sev != "" || ts != "" {
			tr.Parsed++
		}
		if unit != "" {
			units[unit] = struct{}{}
		}
		c, ok := byTmpl[tmpl]
		if !ok {
			c = &Cluster{Template: tmpl, Severity: sev, Unit: unit, FirstLine: lineNo, Example: capLine(raw)}
			byTmpl[tmpl] = c
			order = append(order, tmpl)
		}
		c.Count++
		c.LastLine = lineNo
		if c.Severity == "" && sev != "" {
			c.Severity = sev
		}
	}
	tr.Lines = lineNo
	tr.ParseFallback = tr.Parsed == 0 && lineNo > 0

	clusters := make([]Cluster, 0, len(order))
	for _, tmpl := range order {
		clusters = append(clusters, *byTmpl[tmpl])
	}
	// Rank: errors/criticals first, then by count, then earliest line for a
	// stable order regardless of map iteration.
	sort.SliceStable(clusters, func(i, j int) bool {
		si, sj := severityRank(clusters[i].Severity), severityRank(clusters[j].Severity)
		if si != sj {
			return si > sj
		}
		if clusters[i].Count != clusters[j].Count {
			return clusters[i].Count > clusters[j].Count
		}
		return clusters[i].FirstLine < clusters[j].FirstLine
	})
	for _, c := range clusters {
		if c.Count >= burstThreshold {
			tr.Bursts++
		}
	}
	if len(clusters) > maxClusters {
		for _, c := range clusters[maxClusters:] {
			tr.OmittedLines += c.Count
		}
		clusters = clusters[:maxClusters]
	}
	tr.Clusters = clusters
	tr.TimeStart, tr.TimeEnd = timeStart, timeEnd
	tr.Units = sortedKeys(units, 20)
	return tr
}

// parseLine extracts (timestamp, severity, unit, normalized-template) from one
// log line. Any field may be empty when not detectable.
func parseLine(line string) (ts, severity, unit, template string) {
	rest := line
	if m := reISOTS.FindString(rest); m != "" {
		ts = strings.TrimSpace(m)
		rest = rest[len(m):]
	} else if m := reSyslogTS.FindString(rest); m != "" {
		ts = strings.TrimSpace(m)
		rest = rest[len(m):]
	}
	if m := reSeverity.FindString(line); m != "" {
		severity = canonSeverity(m)
	}
	if m := reUnitPID.FindStringSubmatch(line); len(m) == 2 {
		unit = m[1]
	} else if m := reUnit.FindStringSubmatch(line); len(m) == 2 {
		unit = m[1]
	}
	template = normalizeTemplate(rest)
	return ts, severity, unit, template
}

// normalizeTemplate collapses volatile tokens (ids, ips, paths, numbers,
// quoted strings) into placeholders so similar lines group together.
func normalizeTemplate(s string) string {
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reIP.ReplaceAllString(s, "<ip>")
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reHexBlob.ReplaceAllString(s, "<hex>")
	s = rePath.ReplaceAllString(s, "<path>")
	s = reQuoted.ReplaceAllString(s, `"<str>"`)
	s = reNum.ReplaceAllString(s, "<n>")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxExcerptLine {
		s = s[:maxExcerptLine]
	}
	if s == "" {
		s = "<empty>"
	}
	return s
}

func canonSeverity(s string) string {
	switch strings.ToLower(s) {
	case "emerg", "alert", "crit", "critical", "fatal":
		return "critical"
	case "error", "err":
		return "error"
	case "warning", "warn":
		return "warn"
	case "notice", "info":
		return "info"
	default:
		return "debug"
	}
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 4
	case "error":
		return 3
	case "warn":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

func capLine(s string) string {
	if len(s) > maxExcerptLine {
		return s[:maxExcerptLine] + "…"
	}
	return s
}

func headTail(data []byte) (head, tail []string) {
	lines := []string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		t := sc.Text()
		if strings.TrimSpace(t) == "" {
			continue
		}
		lines = append(lines, capLine(t))
	}
	if len(lines) <= firstLastN {
		return lines, nil
	}
	head = append(head, lines[:firstLastN]...)
	tail = append(tail, lines[len(lines)-firstLastN:]...)
	return head, tail
}

func sortedKeys(m map[string]struct{}, limit int) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
