package net

import (
	"context"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func TestParseSS(t *testing.T) {
	body := `Netid  State    Recv-Q  Send-Q  Local Address:Port  Peer Address:Port  Process
tcp    LISTEN   0       128     0.0.0.0:22          0.0.0.0:*          users:(("sshd",pid=1234,fd=3))
udp    UNCONN   0       0       *:68                *:*                users:(("dhclient",pid=987,fd=6))
tcp    LISTEN   0       4096    [::]:8080           [::]:*
`
	got := parseSS(body)
	if len(got) != 3 {
		t.Fatalf("want 3 sockets, got %d", len(got))
	}
	if got[0].Proto != "tcp" || got[0].Local != "0.0.0.0:22" || !strings.Contains(got[0].Process, "sshd") {
		t.Fatalf("first row: %+v", got[0])
	}
	if got[2].Process != "" {
		t.Fatalf("last row should have empty process, got %q", got[2].Process)
	}
}

func TestDNSResolveLocalhost(t *testing.T) {
	c := dnsResolve{}
	res, err := c.Run(context.Background(), collect.Params{"host": "localhost"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	data := res.Data.(DNSResult)
	if len(data.Addresses) == 0 {
		t.Fatalf("expected addresses for localhost, got %+v", data)
	}
	hasLoopback := false
	for _, a := range data.Addresses {
		if a == "127.0.0.1" || a == "::1" {
			hasLoopback = true
			break
		}
	}
	if !hasLoopback {
		t.Fatalf("loopback not in resolved addresses: %v", data.Addresses)
	}
}

func TestDNSResolveMissingParam(t *testing.T) {
	c := dnsResolve{}
	if _, err := c.Run(context.Background(), collect.Params{}); err == nil {
		t.Fatal("expected error for missing host")
	}
}

func TestNetConnectMissingParam(t *testing.T) {
	c := netConnect{}
	if _, err := c.Run(context.Background(), collect.Params{}); err == nil {
		t.Fatal("expected error for missing targets")
	}
}

func TestNetConnectFailingTarget(t *testing.T) {
	c := netConnect{}
	res, err := c.Run(context.Background(), collect.Params{"targets": "127.0.0.1:1", "timeout": "200ms"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	data := res.Data.(ConnectResult)
	if len(data.Probes) != 1 || data.Probes[0].OK {
		t.Fatalf("expected one failed probe, got %+v", data)
	}
	if len(res.Hints) == 0 {
		t.Fatal("expected hint on failed connect")
	}
}
