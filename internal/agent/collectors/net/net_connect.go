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

	targets := []string{}
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			targets = append(targets, t)
		}
	}

	out := ConnectResult{Probes: make([]Probe, 0, len(targets))}
	hints := []collect.Hint{}

	for _, target := range targets {
		probe := Probe{Target: target}
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
