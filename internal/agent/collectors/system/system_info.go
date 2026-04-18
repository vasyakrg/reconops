// Package system collects host-level facts that need no exec — everything is
// read directly from /proc and /etc.
package system

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func init() { collect.Register(&systemInfo{}) }

type systemInfo struct{}

type SystemInfo struct {
	Distro     string  `json:"distro"`
	OSVersion  string  `json:"os_version"`
	Kernel     string  `json:"kernel"`
	Hostname   string  `json:"hostname"`
	UptimeSec  float64 `json:"uptime_sec"`
	Load1      float64 `json:"load1"`
	Load5      float64 `json:"load5"`
	Load15     float64 `json:"load15"`
	CPUCount   int     `json:"cpu_count"`
	MemTotalKB uint64  `json:"mem_total_kb"`
	MemAvailKB uint64  `json:"mem_avail_kb"`
}

func (systemInfo) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "system_info",
		Version:     "1.0.0",
		Category:    "system",
		Description: "OS distribution, kernel, hostname, uptime, load averages, CPU count, memory totals — read from /proc and /etc/os-release with no exec.",
		Reads:       []string{"/etc/os-release", "/proc/uptime", "/proc/loadavg", "/proc/meminfo", "/proc/cpuinfo", "uname(2)"},
	}
}

func (systemInfo) Run(_ context.Context, _ collect.Params) (collect.Result, error) {
	out := SystemInfo{CPUCount: runtime.NumCPU()}

	if name, _ := os.Hostname(); name != "" {
		out.Hostname = name
	}

	if dist, ver := parseOSRelease("/etc/os-release"); dist != "" {
		out.Distro, out.OSVersion = dist, ver
	}

	if k, err := unameRelease(); err == nil {
		out.Kernel = k
	}

	if up, err := readUptime("/proc/uptime"); err == nil {
		out.UptimeSec = up
	}

	if l1, l5, l15, err := readLoadavg("/proc/loadavg"); err == nil {
		out.Load1, out.Load5, out.Load15 = l1, l5, l15
	}

	if total, avail, err := readMeminfo("/proc/meminfo"); err == nil {
		out.MemTotalKB, out.MemAvailKB = total, avail
	}

	hints := []collect.Hint{}
	if out.MemTotalKB > 0 && out.MemAvailKB*10 < out.MemTotalKB { // <10% free
		hints = append(hints, collect.Hint{
			Severity: "warn", Code: "memory.pressure",
			Message:  "less than 10% memory available",
			Evidence: map[string]uint64{"total_kb": out.MemTotalKB, "avail_kb": out.MemAvailKB},
		})
	}

	return collect.Result{Data: out, Hints: hints}, nil
}

func parseOSRelease(path string) (distro, version string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "ID=") {
			distro = unquote(strings.TrimPrefix(line, "ID="))
		}
		if strings.HasPrefix(line, "VERSION_ID=") {
			version = unquote(strings.TrimPrefix(line, "VERSION_ID="))
		}
	}
	return
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func readUptime(path string) (float64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	parts := strings.Fields(string(body))
	if len(parts) == 0 {
		return 0, nil
	}
	return strconv.ParseFloat(parts[0], 64)
}

func readLoadavg(path string) (float64, float64, float64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	parts := strings.Fields(string(body))
	if len(parts) < 3 {
		return 0, 0, 0, nil
	}
	a, _ := strconv.ParseFloat(parts[0], 64)
	b, _ := strconv.ParseFloat(parts[1], 64)
	c, _ := strconv.ParseFloat(parts[2], 64)
	return a, b, c, nil
}

func readMeminfo(path string) (total, avail uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = parseKB(line)
		}
	}
	return total, avail, nil
}

func parseKB(line string) uint64 {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(parts[1], 10, 64)
	return v
}

// unameRelease is platform-specific (see system_info_linux.go /
// system_info_other.go) — agents run on Linux in production; the non-Linux
// fallback exists so the dev machine can build & lint.
