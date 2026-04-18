package files

import (
	"context"
	"strconv"
	"testing"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func TestPathAllowed(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/etc/os-release", true},
		{"/proc/cpuinfo", true},
		{"/var/log/syslog", true},
		{"/run/systemd/journal", true},
		{"/etc/shadow", false},
		{"/etc/recon/agent.yaml", false},
		{"/etc/ssl/private/server.key", false},
		{"/home/user/.ssh/id_rsa", false},
		{"/", false},
	}
	for _, c := range cases {
		if got := pathAllowed(c.path); got != c.want {
			t.Errorf("pathAllowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestFileReadAllowedPath(t *testing.T) {
	c := fileRead{}
	res, err := c.Run(context.Background(), collect.Params{"path": "/etc/hosts", "max_bytes": "256"})
	if err != nil {
		t.Skipf("skipping — /etc/hosts unreadable on this host: %v", err)
	}
	d := res.Data.(FileResult)
	if d.SHA256 == "" || d.SizeB <= 0 {
		t.Fatalf("unexpected: %+v", d)
	}
}

func TestFileReadDenied(t *testing.T) {
	c := fileRead{}
	if _, err := c.Run(context.Background(), collect.Params{"path": "/etc/shadow"}); err == nil {
		t.Fatal("expected denylist hit")
	}
	if _, err := c.Run(context.Background(), collect.Params{"path": "/home/user/.bashrc"}); err == nil {
		t.Fatal("expected allowlist miss")
	}
	if _, err := c.Run(context.Background(), collect.Params{"path": "../etc/passwd"}); err == nil {
		t.Fatal("expected absolute-path requirement")
	}
}

func TestFileReadCaps(t *testing.T) {
	c := fileRead{}
	// max_bytes way too high: parameter is silently capped.
	res, err := c.Run(context.Background(), collect.Params{"path": "/etc/hosts", "max_bytes": strconv.Itoa(1 << 30)})
	if err != nil {
		t.Skipf("skipping — /etc/hosts unreadable: %v", err)
	}
	d := res.Data.(FileResult)
	if d.Bytes > 1024*1024 {
		t.Fatalf("max_bytes not capped: %d", d.Bytes)
	}
}
