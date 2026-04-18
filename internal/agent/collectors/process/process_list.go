// Package process implements the process_list collector.
package process

import (
	"context"
	"sort"
	"strconv"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func init() { collect.Register(&processList{}) }

type processList struct{}

type Process struct {
	PID    int    `json:"pid"`
	PPID   int    `json:"ppid"`
	Comm   string `json:"comm"`
	State  string `json:"state"`
	CPUS   uint64 `json:"cpu_ticks"`
	RSSKB  uint64 `json:"rss_kb"`
	VMSize uint64 `json:"vm_kb"`
	UID    int    `json:"uid"`
}

type ProcessResult struct {
	Total    int       `json:"total"`
	TopByCPU []Process `json:"top_by_cpu"`
	TopByRSS []Process `json:"top_by_rss"`
	Zombies  []Process `json:"zombies,omitempty"`
}

func (processList) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "process_list",
		Version:     "1.0.0",
		Category:    "process",
		Description: "Process snapshot from /proc/*/stat + /proc/*/status. Returns total count and top-N by CPU and RSS, plus zombies.",
		Reads:       []string{"/proc/*/stat", "/proc/*/status"},
		ParamsSchema: []collect.ParamSpec{
			{Name: "top_n", Type: "int", Default: "10", Description: "how many entries in top_by_cpu/top_by_rss"},
		},
	}
}

func (processList) Run(_ context.Context, p collect.Params) (collect.Result, error) {
	topN := 10
	if s := p["top_n"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 200 {
			topN = n
		}
	}

	procs, err := readProcesses("/proc")
	if err != nil {
		return collect.Result{}, err
	}
	out := ProcessResult{Total: len(procs)}

	byCPU := append([]Process(nil), procs...)
	sort.Slice(byCPU, func(i, j int) bool { return byCPU[i].CPUS > byCPU[j].CPUS })
	out.TopByCPU = takeN(byCPU, topN)

	byRSS := append([]Process(nil), procs...)
	sort.Slice(byRSS, func(i, j int) bool { return byRSS[i].RSSKB > byRSS[j].RSSKB })
	out.TopByRSS = takeN(byRSS, topN)

	for _, pr := range procs {
		if pr.State == "Z" {
			out.Zombies = append(out.Zombies, pr)
		}
	}

	hints := []collect.Hint{}
	if len(out.Zombies) >= 5 {
		hints = append(hints, collect.Hint{
			Severity: "warn", Code: "process.zombies",
			Message:  "more than 5 zombie processes",
			Evidence: map[string]any{"count": len(out.Zombies)},
		})
	}

	return collect.Result{Data: out, Hints: hints}, nil
}

func takeN(p []Process, n int) []Process {
	if n > len(p) {
		n = len(p)
	}
	return p[:n]
}
