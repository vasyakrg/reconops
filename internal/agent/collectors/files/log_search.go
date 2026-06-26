// log_search greps a regex across one or more allowlisted files ON THE HOST
// and returns matching lines with context. It runs entirely in-process via Go's
// RE2 engine over os.Open'd handles — it never shells out to grep(1), so it
// needs no exec whitelist / sudoers entry and trips none of the read-only lint.
// It reuses file_read's allowlist + symlink guard (guardPath) on every path it
// touches, so a path_glob can never be tricked into reading through a symlink
// or outside the allowlist. Matching megabytes stay on the host; only the
// bounded match set crosses into the investigator context.
package files

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func init() { collect.Register(&logSearch{}) }

type logSearch struct{}

const (
	logSearchMaxScanPerFile = 16 * 1024 * 1024 // bound memory per file
	logSearchMaxFiles       = 64               // bound path_glob fan-out
	logSearchDefaultMatches = 50
	logSearchMaxMatches     = 500
	logSearchMaxLineChars   = 1000
)

type LogSearchMatch struct {
	File    string   `json:"file"`
	LineNo  int      `json:"line"`
	Text    string   `json:"text"`
	Context []string `json:"context,omitempty"`
}

type LogSearchSummary struct {
	Pattern      string `json:"pattern"`
	FilesScanned int    `json:"files_scanned"`
	FilesMatched int    `json:"files_matched"`
	Matches      int    `json:"matches"`
	Truncated    bool   `json:"truncated"`
	FromEnd      bool   `json:"from_end,omitempty"`
	Since        string `json:"since,omitempty"`
	Until        string `json:"until,omitempty"`
	Artifact     string `json:"artifact"`
}

func (logSearch) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "log_search",
		Version:     "1.0.0",
		Category:    "files",
		Description: "Grep a regex (RE2) across one or more allowlisted log files ON THE HOST and return matching lines with context — megabytes never enter the investigator context. Supports `path` or `path_glob`, an optional time window (since/until, best-effort on leading log timestamps), and `from_end` to scan only the recent tail of large files. Reuses file_read's allowlist + symlink guard. Add (?i) to the pattern for case-insensitive matching. Prefer this over file_read for large files (>4 MiB), where search_artifact only sees the prefix.",
		Reads:       []string{"allowlisted file paths only"},
		ParamsSchema: []collect.ParamSpec{
			{Name: "path", Type: "string", Description: "absolute path within the allowlist (use this OR path_glob)"},
			{Name: "path_glob", Type: "string", Description: "glob within the allowlist, e.g. /var/log/*.log (use this OR path); matches outside the allowlist or via symlinks are skipped"},
			{Name: "pattern", Type: "string", Required: true, Description: "RE2 regex; add (?i) for case-insensitive"},
			{Name: "since", Type: "string", Description: "only lines at/after this time (RFC3339 or '2006-01-02 15:04:05'); best-effort on leading timestamps"},
			{Name: "until", Type: "string", Description: "only lines at/before this time; best-effort"},
			{Name: "context_lines", Type: "int", Default: "3", Description: "context lines per match (cap 20)"},
			{Name: "max_matches", Type: "int", Default: "50", Description: "cap on returned matches (cap 500)"},
			{Name: "from_end", Type: "bool", Default: "false", Description: "scan only the last 16 MiB of each file (the recent tail); line numbers are then window-relative"},
		},
	}
}

func (logSearch) Run(_ context.Context, p collect.Params) (collect.Result, error) {
	pattern := strings.TrimSpace(p["pattern"])
	if pattern == "" {
		return collect.Result{}, fmt.Errorf("pattern parameter required")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return collect.Result{}, fmt.Errorf("invalid RE2 pattern: %w", err)
	}

	files, err := logSearchFiles(p)
	if err != nil {
		return collect.Result{}, err
	}
	if len(files) == 0 {
		return collect.Result{}, fmt.Errorf("no readable allowlisted files matched path/path_glob")
	}

	fromEnd := p["from_end"] == "true" || p["from_end"] == "1"
	contextLines := clampInt(p["context_lines"], 3, 0, 20)
	maxMatches := clampInt(p["max_matches"], logSearchDefaultMatches, 1, logSearchMaxMatches)

	var since, until time.Time
	var hasSince, hasUntil bool
	if s := strings.TrimSpace(p["since"]); s != "" {
		t, ok := parseFlexibleTime(s)
		if !ok {
			return collect.Result{}, fmt.Errorf("cannot parse since=%q (use RFC3339 or '2006-01-02 15:04:05')", s)
		}
		since, hasSince = t, true
	}
	if s := strings.TrimSpace(p["until"]); s != "" {
		t, ok := parseFlexibleTime(s)
		if !ok {
			return collect.Result{}, fmt.Errorf("cannot parse until=%q (use RFC3339 or '2006-01-02 15:04:05')", s)
		}
		until, hasUntil = t, true
	}

	win := timeWindow{since: since, until: until, hasSince: hasSince, hasUntil: hasUntil}
	var matches []LogSearchMatch
	filesMatched := 0
	truncated := false
	for _, file := range files {
		fm, ferr := logSearchOne(file, re, fromEnd, contextLines, win, maxMatches-len(matches))
		if ferr != nil {
			continue // unreadable file — best-effort across a glob
		}
		if len(fm) > 0 {
			filesMatched++
		}
		matches = append(matches, fm...)
		if len(matches) >= maxMatches {
			truncated = true
			matches = matches[:maxMatches]
			break
		}
	}

	artName := "log_search_" + sanitizeArtifact(logSearchBase(p)) + ".txt"
	summary := LogSearchSummary{
		Pattern: pattern, FilesScanned: len(files), FilesMatched: filesMatched,
		Matches: len(matches), Truncated: truncated, FromEnd: fromEnd, Artifact: artName,
	}
	if hasSince {
		summary.Since = since.Format(time.RFC3339)
	}
	if hasUntil {
		summary.Until = until.Format(time.RFC3339)
	}

	hints := make([]collect.Hint, 0, 5)
	for i, m := range matches {
		if i >= 5 {
			break
		}
		hints = append(hints, collect.Hint{
			Severity: "info", Code: "log_search.match",
			Message: fmt.Sprintf("%s:%d: %s", m.File, m.LineNo, m.Text),
		})
	}

	slog.Debug("collector log_search",
		"pattern", pattern, "files_scanned", len(files), "files_matched", filesMatched,
		"matches", len(matches), "from_end", fromEnd, "truncated", truncated, "artifact", artName)

	return collect.Result{
		Data:      summary,
		Hints:     hints,
		Artifacts: []collect.Artifact{{Name: artName, Mime: "text/plain; charset=utf-8", Body: formatMatches(matches)}},
	}, nil
}

// logSearchFiles resolves path / path_glob to a deduplicated, capped set of
// allowlisted regular files. An explicit `path` that fails the guard is a hard
// error; glob matches that fail (disallowed, symlink, non-regular) are skipped
// so one poisoned entry does not fail the whole sweep — but are never read.
func logSearchFiles(p collect.Params) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(c string, hard bool) error {
		c = filepath.Clean(c)
		if !filepath.IsAbs(c) {
			if hard {
				return fmt.Errorf("path must be absolute: %q", c)
			}
			return nil
		}
		if err := guardPath(c); err != nil {
			if hard {
				return err
			}
			return nil
		}
		if fi, err := os.Lstat(c); err != nil || !fi.Mode().IsRegular() {
			if hard {
				return fmt.Errorf("not a regular file: %q", c)
			}
			return nil
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
		return nil
	}

	if path := strings.TrimSpace(p["path"]); path != "" {
		if err := add(path, true); err != nil {
			return nil, err
		}
	}
	if glob := strings.TrimSpace(p["path_glob"]); glob != "" {
		ms, err := filepath.Glob(glob)
		if err != nil {
			return nil, fmt.Errorf("bad path_glob: %w", err)
		}
		for _, m := range ms {
			if len(out) >= logSearchMaxFiles {
				break
			}
			_ = add(m, false)
		}
	}
	if len(out) == 0 && strings.TrimSpace(p["path"]) == "" && strings.TrimSpace(p["path_glob"]) == "" {
		return nil, fmt.Errorf("path or path_glob required")
	}
	sort.Strings(out)
	return out, nil
}

type timeWindow struct {
	since, until       time.Time
	hasSince, hasUntil bool
}

func (w timeWindow) active() bool { return w.hasSince || w.hasUntil }

func (w timeWindow) contains(line string, ref time.Time) bool {
	if !w.active() {
		return true
	}
	t, ok := parseLogLineTime(line, ref)
	if !ok {
		return true // can't time-filter a line without a parseable timestamp — keep it
	}
	if w.hasSince && t.Before(w.since) {
		return false
	}
	if w.hasUntil && t.After(w.until) {
		return false
	}
	return true
}

func logSearchOne(file string, re *regexp.Regexp, fromEnd bool, contextLines int, win timeWindow, limit int) ([]LogSearchMatch, error) {
	if limit <= 0 {
		return nil, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	var start int64
	readLen := size
	if size > logSearchMaxScanPerFile {
		readLen = logSearchMaxScanPerFile
		if fromEnd {
			start = size - logSearchMaxScanPerFile
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, readLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	data := buf[:n]
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:] // drop the partial leading line
		}
	}
	lines := strings.Split(string(data), "\n")
	ref := time.Now().UTC()
	var out []LogSearchMatch
	for i, ln := range lines {
		if !re.MatchString(ln) {
			continue
		}
		if !win.contains(ln, ref) {
			continue
		}
		m := LogSearchMatch{File: file, LineNo: i + 1, Text: capLine(ln)}
		if contextLines > 0 {
			lo := i - contextLines
			if lo < 0 {
				lo = 0
			}
			hi := i + contextLines + 1
			if hi > len(lines) {
				hi = len(lines)
			}
			for _, c := range lines[lo:hi] {
				m.Context = append(m.Context, capLine(c))
			}
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func formatMatches(matches []LogSearchMatch) []byte {
	var b strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&b, "%s:%d: %s\n", m.File, m.LineNo, m.Text)
		for _, c := range m.Context {
			fmt.Fprintf(&b, "    | %s\n", c)
		}
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func logSearchBase(p collect.Params) string {
	if path := strings.TrimSpace(p["path"]); path != "" {
		return filepath.Base(path)
	}
	return filepath.Base(strings.TrimSpace(p["path_glob"]))
}

func capLine(s string) string {
	if len(s) <= logSearchMaxLineChars {
		return s
	}
	return s[:logSearchMaxLineChars] + "…"
}

func clampInt(s string, def, lo, hi int) int {
	v := def
	if s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			v = n
		}
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}

var flexibleTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseFlexibleTime(s string) (time.Time, bool) {
	for _, l := range flexibleTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// parseLogLineTime best-effort extracts a leading timestamp from a log line by
// tokenizing on whitespace (robust to RFC3339's variable-length zone and to
// syslog's space-padded day). Absolute layouts parse directly; the year-less
// syslog layout assumes the reference year (current year on the host).
func parseLogLineTime(line string, ref time.Time) (time.Time, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return time.Time{}, false
	}
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(l, fields[0]); err == nil {
			return t.UTC(), true
		}
	}
	if len(fields) >= 2 {
		if t, err := time.Parse("2006-01-02 15:04:05", fields[0]+" "+fields[1]); err == nil {
			return t.UTC(), true
		}
	}
	if len(fields) >= 3 {
		cand := fields[0] + " " + fields[1] + " " + fields[2]
		if t, err := time.Parse("Jan 2 15:04:05", cand); err == nil {
			return time.Date(ref.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC), true
		}
	}
	return time.Time{}, false
}
