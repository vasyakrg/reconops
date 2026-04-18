package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/exec"
)

func init() { collect.Register(&journalTail{}) }

type journalTail struct{}

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
		Version:     "1.0.0",
		Category:    "systemd",
		Description: "Tail of journalctl for one unit. Returns a compact summary in data; full line-delimited JSON goes into an artifact for grep via search_artifact.",
		Reads:       []string{"journalctl -u {unit} --since {since} -n {lines} -o json --no-pager"},
		Requires:    []collect.Capability{collect.CapSudoJournalctl},
		ParamsSchema: []collect.ParamSpec{
			{Name: "unit", Type: "string", Required: true, Description: "systemd unit name (e.g. kubelet.service)"},
			{Name: "since", Type: "string", Default: "1 hour ago", Description: "journalctl --since value"},
			{Name: "lines", Type: "int", Default: "1000", Description: "max lines to return (caps at 100000)"},
		},
	}
}

func (journalTail) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	unit := strings.TrimSpace(p["unit"])
	if unit == "" {
		return collect.Result{}, fmt.Errorf("unit parameter required")
	}
	since := strings.TrimSpace(p["since"])
	if since == "" {
		since = "1 hour ago"
	}
	lines := 1000
	if s := p["lines"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 100000 {
			lines = n
		}
	}

	res, err := exec.Run(ctx, "journalctl",
		[]string{"-u", unit, "--since", since, "-n", strconv.Itoa(lines), "-o", "json", "--no-pager"})
	truncated := errors.Is(err, exec.ErrStdoutTruncated)
	if err != nil && !truncated {
		return collect.Result{}, fmt.Errorf("journalctl: %w", err)
	}

	summary, hints := summarizeJournal(unit, since, res.Stdout)
	artName := fmt.Sprintf("journal_%s.jsonl", sanitizeUnit(unit))
	summary.ArtifactName = artName
	if truncated {
		hints = append(hints, collect.Hint{
			Severity: "warn", Code: "journal.truncated",
			Message: "journal output exceeded 16 MiB cap and was truncated; reduce --lines or narrow --since",
		})
	}

	return collect.Result{
		Data:  summary,
		Hints: hints,
		Artifacts: []collect.Artifact{
			{Name: artName, Mime: "application/x-ndjson", Body: res.Stdout},
		},
	}, nil
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
