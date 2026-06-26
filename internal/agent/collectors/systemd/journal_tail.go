package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/exec"
)

func init() { collect.Register(&journalTail{}) }

type journalTail struct{}

func (journalTail) Available() bool { return exec.BinaryAvailable("journalctl") }

// JournalEntry is the small subset of fields we surface in the summary;
// the full per-line records go into the artifact body.
type JournalEntry struct {
	Timestamp string `json:"__REALTIME_TIMESTAMP"`
	Unit      string `json:"_SYSTEMD_UNIT"`
	Priority  string `json:"PRIORITY"`
	Message   string `json:"MESSAGE"`
}

type JournalSummary struct {
	Unit         string         `json:"unit"`
	Source       string         `json:"source"`
	Since        string         `json:"since"`
	Lines        int            `json:"lines"`
	Errors       int            `json:"errors"`
	Warnings     int            `json:"warnings"`
	Levels       map[string]int `json:"by_level"`
	ArtifactName string         `json:"artifact"`
}

func (journalTail) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "journal_tail",
		Version:     "1.1.0",
		Category:    "systemd",
		Description: "Read the systemd journal. Tail one unit, the kernel ring (kernel=true — where TPM/hardware spam lives), or the whole journal; optionally the previous boot (previous_boot=true — pre-reboot logs), a time window (since/until), a priority filter (priority), or a regex (grep). Returns a compact summary; full line-delimited JSON goes into an artifact for grep via search_artifact. When a unit's journal is empty the host's journald may be volatile — fall back to log_search / file_read over /var/log and the kernel ring.",
		Reads:       []string{"journalctl -u {unit} | -k [-b -1] [--since ..] [--until ..] [-p ..] [-g ..] -n {lines} -o json --no-pager"},
		Requires:    []collect.Capability{collect.CapSudoJournalctl},
		ParamsSchema: []collect.ParamSpec{
			{Name: "unit", Type: "string", Description: "systemd unit name (e.g. kubelet.service). Omit for the whole journal; mutually exclusive with kernel."},
			{Name: "kernel", Type: "bool", Default: "false", Description: "read the kernel ring buffer (-k / --dmesg) instead of a unit"},
			{Name: "previous_boot", Type: "bool", Default: "false", Description: "read the PREVIOUS boot (-b -1) — the logs from before the last reboot"},
			{Name: "since", Type: "string", Default: "1 hour ago", Description: "journalctl --since value"},
			{Name: "until", Type: "string", Description: "journalctl --until value (upper time bound)"},
			{Name: "priority", Type: "string", Description: "journalctl -p priority filter: 0-7, a name (err), or a range (0..3)"},
			{Name: "grep", Type: "string", Description: "journalctl -g regex filter on the message"},
			{Name: "lines", Type: "int", Default: "1000", Description: "max lines to return (caps at 100000)"},
		},
	}
}

func (journalTail) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	unit := strings.TrimSpace(p["unit"])
	kernel := p["kernel"] == "true" || p["kernel"] == "1"
	prevBoot := p["previous_boot"] == "true" || p["previous_boot"] == "1"
	since := strings.TrimSpace(p["since"])
	until := strings.TrimSpace(p["until"])
	priority := strings.TrimSpace(p["priority"])
	grep := strings.TrimSpace(p["grep"])

	if unit != "" && kernel {
		return collect.Result{}, fmt.Errorf("unit and kernel are mutually exclusive")
	}
	// A bare unit tail keeps its historical default window; the other modes
	// (kernel, whole journal) do not force a --since so the model controls it.
	if since == "" && unit != "" && !prevBoot {
		since = "1 hour ago"
	}

	lines := 1000
	if s := p["lines"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 100000 {
			lines = n
		}
	}

	// Canonical arg order — MUST match journalctlPatterns() in the exec
	// whitelist, or exec.Run panics on an unmatched shape.
	args := make([]string, 0, 16)
	if kernel {
		args = append(args, "-k")
	} else if unit != "" {
		args = append(args, "-u", unit)
	}
	if prevBoot {
		args = append(args, "-b", "-1")
	}
	if since != "" {
		args = append(args, "--since", since)
	}
	if until != "" {
		args = append(args, "--until", until)
	}
	if priority != "" {
		args = append(args, "-p", priority)
	}
	if grep != "" {
		args = append(args, "-g", grep)
	}
	args = append(args, "-n", strconv.Itoa(lines), "-o", "json", "--no-pager")

	res, err := exec.Run(ctx, "journalctl", args)
	truncated := errors.Is(err, exec.ErrStdoutTruncated)
	if err != nil && !truncated {
		return collect.Result{}, fmt.Errorf("journalctl: %w", err)
	}

	summary, hints := summarizeJournal(unit, since, res.Stdout)
	summary.Source = journalSource(unit, kernel, prevBoot)
	artName := journalArtifactName(unit, kernel, prevBoot)
	summary.ArtifactName = artName
	if truncated {
		hints = append(hints, collect.Hint{
			Severity: "warn", Code: "journal.truncated",
			Message: "journal output exceeded 16 MiB cap and was truncated; reduce --lines or narrow --since/--until",
		})
	}
	if summary.Lines == 0 {
		hints = append(hints, collect.Hint{
			Severity: "info", Code: "journal.empty",
			Message: "no journal entries for this selector — journald may be volatile on this host. Fall back to log_search/file_read(from_end) over /var/log/syslog and to the kernel ring (kernel=true or the dmesg collector).",
		})
	}

	slog.Debug("collector journal_tail",
		"source", summary.Source, "since", since, "until", until, "priority", priority,
		"grep", grep != "", "previous_boot", prevBoot, "lines_returned", summary.Lines, "artifact", artName)

	return collect.Result{
		Data:  summary,
		Hints: hints,
		Artifacts: []collect.Artifact{
			{Name: artName, Mime: "application/x-ndjson", Body: res.Stdout},
		},
	}, nil
}

// journalSource is a human-readable label for the journal selection.
func journalSource(unit string, kernel, prevBoot bool) string {
	s := "all"
	switch {
	case kernel:
		s = "kernel"
	case unit != "":
		s = "unit:" + unit
	}
	if prevBoot {
		s += " (previous boot)"
	}
	return s
}

// journalArtifactName derives a distinct, sanitize()-safe artifact name per
// query mode so kernel-ring / previous-boot / per-unit calls never collide on
// "journal_.jsonl" (which would break the search_artifact name listing).
func journalArtifactName(unit string, kernel, prevBoot bool) string {
	base := "all"
	switch {
	case kernel:
		base = "kernel"
	case unit != "":
		base = sanitizeUnit(unit)
	}
	name := "journal_" + base
	if prevBoot {
		name += "_boot-1"
	}
	return name + ".jsonl"
}

// summarizeJournal counts entries by priority level and surfaces the top-N
// errors/warnings as hints. Priority strings follow journalctl's conventions:
//
//	"0" emerg .. "3" err .. "4" warn .. "7" debug
func summarizeJournal(unit, since string, body []byte) (JournalSummary, []collect.Hint) {
	out := JournalSummary{
		Unit: unit, Since: since,
		Levels: map[string]int{},
	}
	hints := []collect.Hint{}
	const maxHints = 5

	for _, raw := range strings.Split(string(body), "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		out.Lines++
		var e JournalEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			continue
		}
		out.Levels[e.Priority]++
		switch e.Priority {
		case "0", "1", "2", "3":
			out.Errors++
			if len(hints) < maxHints {
				hints = append(hints, collect.Hint{
					Severity: "error", Code: "journal.error_line",
					Message: truncate(e.Message, 240),
				})
			}
		case "4":
			out.Warnings++
		}
	}
	return out, hints
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sanitizeUnit(u string) string {
	out := make([]byte, 0, len(u))
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
