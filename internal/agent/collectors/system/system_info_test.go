package system

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseOSRelease(t *testing.T) {
	p := writeFixture(t, "os-release", `NAME="Ubuntu"
ID=ubuntu
VERSION_ID="22.04"
PRETTY_NAME="Ubuntu 22.04 LTS"
`)
	d, v := parseOSRelease(p)
	if d != "ubuntu" || v != "22.04" {
		t.Fatalf("got distro=%q ver=%q", d, v)
	}
}

func TestReadUptime(t *testing.T) {
	p := writeFixture(t, "uptime", "12345.67 9876.54\n")
	got, err := readUptime(p)
	if err != nil || got != 12345.67 {
		t.Fatalf("got %v err=%v", got, err)
	}
}

func TestReadLoadavg(t *testing.T) {
	p := writeFixture(t, "loadavg", "0.42 0.55 0.61 1/234 5678\n")
	a, b, c, err := readLoadavg(p)
	if err != nil {
		t.Fatal(err)
	}
	if a != 0.42 || b != 0.55 || c != 0.61 {
		t.Fatalf("got %v %v %v", a, b, c)
	}
}

func TestReadMeminfo(t *testing.T) {
	p := writeFixture(t, "meminfo", `MemTotal:       16384000 kB
MemFree:         2000000 kB
MemAvailable:    8192000 kB
Buffers:          100000 kB
`)
	total, avail, err := readMeminfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if total != 16384000 || avail != 8192000 {
		t.Fatalf("got total=%d avail=%d", total, avail)
	}
}

func TestManifestStable(t *testing.T) {
	m := (systemInfo{}).Manifest()
	if m.Name != "system_info" || m.Category != "system" || len(m.Reads) == 0 {
		t.Fatalf("manifest looks wrong: %+v", m)
	}
}
