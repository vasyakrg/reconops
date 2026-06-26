package system

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/exec"
)

func init() { collect.Register(&dmesgCollector{}) }

type dmesgCollector struct{}

func (dmesgCollector) Available() bool { return exec.BinaryAvailable("dmesg") }

type DmesgSummary struct {
	Lines      int    `json:"lines"`
	Matched    int    `json:"matched,omitempty"`
	Pattern    string `json:"pattern,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Restricted bool   `json:"restricted,omitempty"`
	Artifact   string `json:"artifact"`
}

func (dmesgCollector) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "dmesg",
		Version:     "1.0.0",
		Category:    "system",
		Description: "Read the kernel ring buffer (dmesg) with ABSOLUTE ISO timestamps, so kernel/hardware messages can be anchored to an incident's wall-clock time. The primary source for kernel spam (e.g. TPM timeouts) on hosts whose journald is volatile. Optional in-process regex (grep); full output goes to a searchable artifact.",
		Reads:       []string{"dmesg --time-format=iso"},
		Requires:    []collect.Capability{collect.CapSyslog},
		ParamsSchema: []collect.ParamSpec{
			{Name: "grep", Type: "string", Description: "RE2 regex to filter lines in-process; add (?i) for case-insensitive"},
			{Name: "max_lines", Type: "int", Default: "2000", Description: "cap on returned lines (cap 100000)"},
		},
	}
}

func (dmesgCollector) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	res, err := exec.Run(ctx, "dmesg", []string{"--time-format=iso"})
	truncatedStdout := errors.Is(err, exec.ErrStdoutTruncated)
	if err != nil && !truncatedStdout {
		return collect.Result{}, fmt.Errorf("dmesg: %w", err)
	}
	maxLines := 2000
	if s := p["max_lines"]; s != "" {
		if n, e := strconv.Atoi(s); e == nil && n > 0 && n <= 100000 {
			maxLines = n
		}
	}
	return buildDmesgResult(res.Stdout, res.ExitCode, res.Stderr, truncatedStdout, strings.TrimSpace(p["grep"]), maxLines)
}

// buildDmesgResult is the pure (exec-free) core: parse dmesg stdout, optionally
// grep in-process, cap lines, detect a dmesg_restrict block, and assemble the
// summary + artifact. Split out so it is unit-testable without the binary.
func buildDmesgResult(stdout []byte, exitCode int, stderr []byte, truncatedStdout bool, pattern string, maxLines int) (collect.Result, error) {
	var re *regexp.Regexp
	if pattern != "" {
		var err error
		re, err = regexp.Compile(pattern)
		if err != nil {
			return collect.Result{}, fmt.Errorf("invalid RE2 grep pattern: %w", err)
		}
	}

	// dmesg_restrict=1 → non-zero exit + empty stdout + a permission error.
	restricted := false
	if exitCode != 0 && len(stdout) == 0 {
		se := string(stderr)
		if strings.Contains(se, "Operation not permitted") || strings.Contains(se, "Permission denied") {
			restricted = true
		}
	}

	allLines := splitNonEmptyTail(stdout)
	var kept []string
	matched := 0
	for _, ln := range allLines {
		if re != nil {
			if !re.MatchString(ln) {
				continue
			}
			matched++
		}
		kept = append(kept, ln)
		if len(kept) >= maxLines {
			break
		}
	}
	truncated := truncatedStdout
	if re == nil && len(allLines) > maxLines {
		truncated = true
	} else if re != nil && len(kept) >= maxLines {
		truncated = true
	}

	const artName = "dmesg.txt"
	body := []byte(strings.Join(kept, "\n"))
	if len(kept) > 0 {
		body = append(body, '\n')
	}
	summary := DmesgSummary{Lines: len(kept), Truncated: truncated, Restricted: restricted, Artifact: artName}
	if re != nil {
		summary.Pattern = pattern
		summary.Matched = matched
	}

	var hints []collect.Hint
	if restricted {
		hints = append(hints, collect.Hint{
			Severity: "warn", Code: "dmesg.restricted",
			Message: "dmesg returned no output with a permission error — kernel.dmesg_restrict=1 blocks unprivileged reads; the agent needs CAP_SYSLOG (deploy/systemd/recon-agent.service). Fall back to journal_tail(kernel=true).",
		})
	}

	slog.Debug("collector dmesg",
		"lines", len(kept), "matched", matched, "restricted", restricted,
		"exit_code", exitCode, "artifact", artName)

	return collect.Result{
		Data:      summary,
		Hints:     hints,
		Artifacts: []collect.Artifact{{Name: artName, Mime: "text/plain; charset=utf-8", Body: body}},
	}, nil
}

// splitNonEmptyTail splits stdout into lines, dropping a single trailing empty
// line from the final newline.
func splitNonEmptyTail(stdout []byte) []string {
	s := strings.TrimRight(string(stdout), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
