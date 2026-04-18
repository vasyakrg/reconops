package net

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func init() { collect.Register(&netConnect{}) }

type netConnect struct{}

type Probe struct {
	Target  string        `json:"target"`
	OK      bool          `json:"ok"`
	Latency time.Duration `json:"latency_ns,omitempty"`
	Error   string        `json:"error,omitempty"`
}

type ConnectResult struct {
	Probes []Probe `json:"probes"`
}

func (netConnect) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "net_connect",
		Version:     "1.0.0",
		Category:    "network",
		Description: "TCP-connect probes to a comma-separated list of host:port targets. Pure Go — no exec, no raw sockets.",
		Reads:       []string{"network — outbound TCP connect"},
		ParamsSchema: []collect.ParamSpec{
			{Name: "targets", Type: "string", Required: true, Description: "comma-separated host:port list"},
			{Name: "timeout", Type: "duration", Default: "2s", Description: "per-target connect timeout"},
		},
	}
}

func (netConnect) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	raw := strings.TrimSpace(p["targets"])
	if raw == "" {
		return collect.Result{}, fmt.Errorf("targets parameter required (comma-separated host:port)")
	}
	timeout := 2 * time.Second
	if s := p["timeout"]; s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 && d <= 30*time.Second {
			timeout = d
		}
	}

	const maxTargets = 16
	targets := []string{}
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			targets = append(targets, t)
		}
	}
	if len(targets) > maxTargets {
		return collect.Result{}, fmt.Errorf("too many targets (%d); max %d", len(targets), maxTargets)
	}

	out := ConnectResult{Probes: make([]Probe, 0, len(targets))}
	hints := []collect.Hint{}

	for _, target := range targets {
		probe := Probe{Target: target}
		if reason := disallowedTarget(target); reason != "" {
			probe.Error = "blocked: " + reason
			out.Probes = append(out.Probes, probe)
			hints = append(hints, collect.Hint{
				Severity: "warn", Code: "net.target_blocked",
				Message: fmt.Sprintf("%s blocked: %s", target, reason),
			})
			continue
		}
		start := time.Now()
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", target)
		probe.Latency = time.Since(start)
		if err != nil {
			probe.Error = err.Error()
			hints = append(hints, collect.Hint{
				Severity: "warn", Code: "net.connect_failed",
				Message:  fmt.Sprintf("%s: %s", target, err),
				Evidence: map[string]any{"target": target, "error": err.Error()},
			})
		} else {
			probe.OK = true
			_ = conn.Close()
		}
		out.Probes = append(out.Probes, probe)
	}

	return collect.Result{Data: out, Hints: hints}, nil
}

// disallowedTarget returns a non-empty reason if the target host:port is on
// the SSRF blocklist: link-local IPv4 / IPv6, cloud metadata endpoints,
// loopback IPv6 link-local. Loopback IPv4 (127.0.0.0/8) is allowed because
// it is a legitimate diagnostic use-case (probe a local service).
func disallowedTarget(target string) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return "invalid host:port"
	}
	if host == "169.254.169.254" || host == "fd00:ec2::254" || host == "metadata.google.internal" {
		return "cloud metadata endpoint"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostname — resolved later by Dial; we cannot pre-check easily.
		return ""
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "link-local address"
	}
	if ip.IsMulticast() {
		return "multicast address"
	}
	if ip.IsUnspecified() {
		return "unspecified address (0.0.0.0 / ::)"
	}
	return ""
}
