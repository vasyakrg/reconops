package net

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func init() { collect.Register(&dnsResolve{}) }

type dnsResolve struct{}

type DNSResult struct {
	Query     string        `json:"query"`
	Addresses []string      `json:"addresses"`
	CNAME     string        `json:"cname,omitempty"`
	Latency   time.Duration `json:"latency_ns"`
	Error     string        `json:"error,omitempty"`
}

func (dnsResolve) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "dns_resolve",
		Version:     "1.0.0",
		Category:    "network",
		Description: "Resolve a hostname using the system resolver. Returns A/AAAA addresses, CNAME, and latency. Pure Go — no exec.",
		Reads:       []string{"system resolver (/etc/resolv.conf, nss)"},
		ParamsSchema: []collect.ParamSpec{
			{Name: "host", Type: "string", Required: true, Description: "FQDN to resolve"},
			{Name: "timeout", Type: "duration", Default: "3s", Description: "Resolver timeout"},
		},
	}
}

func (dnsResolve) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	host := strings.TrimSpace(p["host"])
	if host == "" {
		return collect.Result{}, fmt.Errorf("host parameter required")
	}
	timeout := 3 * time.Second
	if s := p["timeout"]; s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 && d <= 30*time.Second {
			timeout = d
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := &net.Resolver{}
	start := time.Now()
	addrs, err := res.LookupHost(ctx, host)
	latency := time.Since(start)

	out := DNSResult{Query: host, Addresses: addrs, Latency: latency}
	if err != nil {
		out.Error = err.Error()
	}
	if cname, cerr := res.LookupCNAME(ctx, host); cerr == nil && cname != host+"." {
		out.CNAME = strings.TrimSuffix(cname, ".")
	}

	hints := []collect.Hint{}
	if err != nil {
		hints = append(hints, collect.Hint{Severity: "warn", Code: "dns.resolve_failed", Message: err.Error()})
	} else if len(addrs) == 0 {
		hints = append(hints, collect.Hint{Severity: "warn", Code: "dns.empty_result", Message: "resolver returned no addresses"})
	}

	return collect.Result{Data: out, Hints: hints}, nil
}
