//go:build linux

package process

import "testing"

func TestParseStatBasic(t *testing.T) {
	// Synthetic /proc/{pid}/stat line. Fields: pid, comm (in parens with spaces),
	// state, ppid, ..., utime (12), stime (13).
	body := []byte(`1234 (nginx worker) S 1 1234 1234 0 -1 4194304 1000 0 0 0 17 23 0 0 20 0 1 0 100 1024 200 ...`)
	var p Process
	parseStat(body, &p)
	if p.Comm != "nginx worker" {
		t.Fatalf("comm: %q", p.Comm)
	}
	if p.State != "S" || p.PPID != 1 {
		t.Fatalf("state/ppid: %s %d", p.State, p.PPID)
	}
	if p.CPUS != 40 {
		t.Fatalf("cpus: %d", p.CPUS)
	}
}

func TestParseStatusKB(t *testing.T) {
	body := []byte(`Name:	bash
Uid:	1000	1000	1000	1000
VmSize:	   12345 kB
VmRSS:	    6789 kB
`)
	var p Process
	parseStatus(body, &p)
	if p.UID != 1000 || p.VMSize != 12345 || p.RSSKB != 6789 {
		t.Fatalf("got %+v", p)
	}
}
