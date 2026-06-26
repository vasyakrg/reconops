package exec

import (
	"context"
	"testing"
)

func TestNoShellMeta(t *testing.T) {
	bad := []string{"foo;bar", "x|y", "$(date)", "`date`", "a&b", "x>y", "../etc/passwd", "x\nfoo", "*.log"}
	for _, s := range bad {
		if err := NoShellMeta(s); err == nil {
			t.Errorf("expected reject: %q", s)
		}
	}
	good := []string{"foo.bar", "kubelet.service", "2026-04-18T10:00:00Z", "127.0.0.1"}
	for _, s := range good {
		if err := NoShellMeta(s); err != nil {
			t.Errorf("expected allow: %q (%v)", s, err)
		}
	}
}

func TestSystemdUnitName(t *testing.T) {
	bad := []string{"", "../etc", "foo;bar", "foo bar", "foo\x00bar"}
	for _, s := range bad {
		if err := SystemdUnitName(s); err == nil {
			t.Errorf("expected reject: %q", s)
		}
	}
	good := []string{"kubelet.service", "ssh.socket", "user-1000.slice", "getty@tty1.service"}
	for _, s := range good {
		if err := SystemdUnitName(s); err != nil {
			t.Errorf("expected allow: %q (%v)", s, err)
		}
	}
}

func TestJournalSince(t *testing.T) {
	good := []string{"2026-04-18 09:00:00", "1 hour ago", "yesterday", "-15m"}
	for _, s := range good {
		if err := JournalSince(s); err != nil {
			t.Errorf("expected allow: %q (%v)", s, err)
		}
	}
	bad := []string{"$(date)", "x;y", "a|b", "a\nb"}
	for _, s := range bad {
		if err := JournalSince(s); err == nil {
			t.Errorf("expected reject: %q", s)
		}
	}
}

func TestPosInt(t *testing.T) {
	v := PosInt(1000)
	if err := v("0"); err == nil {
		t.Error("expected reject 0")
	}
	if err := v("1001"); err == nil {
		t.Error("expected reject 1001")
	}
	if err := v("foo"); err == nil {
		t.Error("expected reject 'foo'")
	}
	if err := v("500"); err != nil {
		t.Errorf("expected allow 500: %v", err)
	}
}

func TestJournalGrep(t *testing.T) {
	good := []string{"tpm.*timeout", "(error|fail)", "foo|bar", "a.b*c", "\\bpanic\\b"}
	for _, s := range good {
		if err := JournalGrep(s); err != nil {
			t.Errorf("expected allow %q: %v", s, err)
		}
	}
	bad := []string{"", "a\nb", "a\x00b", "(unclosed", string(make([]byte, 513))}
	for _, s := range bad {
		if err := JournalGrep(s); err == nil {
			t.Errorf("expected reject %q", s)
		}
	}
}

func TestPriorityLevel(t *testing.T) {
	good := []string{"0", "7", "err", "warning", "0..3", "warning..debug"}
	for _, s := range good {
		if err := PriorityLevel(s); err != nil {
			t.Errorf("expected allow %q: %v", s, err)
		}
	}
	bad := []string{"8", "-1", "bogus", "", ".."}
	for _, s := range bad {
		if err := PriorityLevel(s); err == nil {
			t.Errorf("expected reject %q", s)
		}
	}
}

func TestJournalctlNewShapes(t *testing.T) {
	resetWhitelist()
	RegisterDefaults()
	mu.RLock()
	entry := whitelist["journalctl"]
	mu.RUnlock()

	allow := [][]string{
		{"-k", "-n", "1000", "-o", "json", "--no-pager"},                                                     // kernel ring
		{"-k", "-b", "-1", "-n", "1000", "-o", "json", "--no-pager"},                                         // kernel + previous boot
		{"-u", "pve-cluster.service", "-b", "-1", "-n", "500", "-o", "json", "--no-pager"},                   // unit + previous boot
		{"--since", "1 hour ago", "--until", "2026-06-20 12:00:00", "-n", "100", "-o", "json", "--no-pager"}, // whole journal window
		{"-u", "kubelet.service", "-p", "err", "-n", "100", "-o", "json", "--no-pager"},                      // priority
		{"-u", "kubelet.service", "-g", "tpm.*timeout", "-n", "100", "-o", "json", "--no-pager"},             // grep
		{"-n", "100", "-o", "json", "--no-pager"},                                                            // whole journal, no filters
		{"-u", "kubelet.service", "--since", "1 hour ago", "-n", "100", "-o", "json", "--no-pager"},          // backward-compatible original shape
	}
	for _, a := range allow {
		if err := validateArgs(entry, a); err != nil {
			t.Errorf("expected allow: %v: %v", a, err)
		}
	}
	deny := [][]string{
		{"-k", "-b", "-2", "-n", "1000", "-o", "json", "--no-pager"},      // only previous boot (-1) allowed
		{"-u", "x", "-g", "a\nb", "-n", "10", "-o", "json", "--no-pager"}, // control char in grep
		{"-f", "-n", "10", "-o", "json", "--no-pager"},                    // -f (follow) is not allowed
		{"-u", "x", "-p", "9", "-n", "10", "-o", "json", "--no-pager"},    // priority out of range
		{"-u", "x", "-o", "csv", "--no-pager", "-n", "10"},                // wrong order / format
	}
	for _, a := range deny {
		if err := validateArgs(entry, a); err == nil {
			t.Errorf("expected reject: %v", a)
		}
	}
}

// TestReadOnlyInvariant_MutatingShapesRejected pins the read-only guarantee at
// layer 3: the kernel-ring collectors run with privilege (CAP_SYSLOG /
// SUDO_JOURNALCTL), so the whitelist MUST reject every mutating arg shape —
// dmesg ring clear / console-loglevel, journalctl --rotate/--vacuum/--flush/
// --sync, and any non-json output mode. If a future edit widens a pattern and
// lets one of these through, this test fails before it can ship.
func TestReadOnlyInvariant_MutatingShapesRejected(t *testing.T) {
	resetWhitelist()
	RegisterDefaults()

	mutating := []struct {
		bin  string
		args []string
		why  string
	}{
		{"dmesg", []string{"-C"}, "clear kernel ring"},
		{"dmesg", []string{"-c"}, "read-and-clear kernel ring"},
		{"dmesg", []string{"--clear"}, "clear kernel ring"},
		{"dmesg", []string{"-n", "1"}, "set console loglevel"},
		{"dmesg", []string{"--console-off"}, "disable console logging"},
		{"dmesg", []string{"--time-format=iso", "-C"}, "read shape + trailing clear"},
		{"journalctl", []string{"--rotate"}, "rotate journal files"},
		{"journalctl", []string{"--vacuum-size=1M"}, "delete old journal data"},
		{"journalctl", []string{"--flush"}, "flush /run journal to /var"},
		{"journalctl", []string{"--sync"}, "force journal write"},
		{"journalctl", []string{"--rotate", "-n", "10", "-o", "json", "--no-pager"}, "rotate smuggled before a read shape"},
		{"journalctl", []string{"-u", "x", "-n", "10", "-o", "export", "--no-pager"}, "non-json output mode"},
	}

	for _, m := range mutating {
		mu.RLock()
		entry := whitelist[m.bin]
		mu.RUnlock()
		if err := validateArgs(entry, m.args); err == nil {
			t.Errorf("read-only breach: %s %v (%s) must be rejected by the whitelist", m.bin, m.args, m.why)
		}
	}

	// And the gateway itself is fail-closed: a rejected shape panics, it does not
	// silently fall through to exec.
	for _, m := range []struct {
		bin  string
		args []string
	}{
		{"dmesg", []string{"-C"}},
		{"journalctl", []string{"--rotate"}},
	} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Run(%s %v) must panic (fail-closed), not proceed", m.bin, m.args)
				}
			}()
			_, _ = Run(context.Background(), m.bin, m.args)
		}()
	}
}

func TestRegisterDefaultsValidation(t *testing.T) {
	resetWhitelist()
	RegisterDefaults()

	// Spot-check a few injection attempts panic.
	cases := []struct {
		name string
		bin  string
		args []string
	}{
		{"journal_unit_inj", "journalctl", []string{"-u", "kubelet.service; rm -rf /", "--since", "1 hour ago", "-n", "10", "-o", "json", "--no-pager"}},
		{"journal_format_inj", "journalctl", []string{"-u", "kubelet.service", "--since", "1 hour ago", "-n", "10", "-o", "csv", "--no-pager"}},
		{"systemctl_extra", "systemctl", []string{"reboot"}},
		{"ss_unknown_flag", "ss", []string{"-K"}},
		{"unknown_bin", "rm", []string{"-rf", "/"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic")
				}
			}()
			_, _ = Run(context.Background(), tc.bin, tc.args)
		})
	}

	// Allowed shapes do NOT panic during validation. We do not actually
	// invoke the binaries (they may not exist in the test sandbox); we
	// only check that validateArgs accepts.
	allowed := []struct {
		bin  string
		args []string
	}{
		{"journalctl", []string{"-u", "kubelet.service", "--since", "1 hour ago", "-n", "100", "-o", "json", "--no-pager"}},
		{"systemctl", []string{"list-units", "--all", "--no-pager", "--no-legend", "-o", "json"}},
		{"ss", []string{"-tulpn"}},
		{"ip", []string{"-json", "addr"}},
		{"iptables", []string{"-L", "-n", "-v"}},
		{"dmesg", []string{"--time-format=iso"}},
	}
	for _, a := range allowed {
		mu.RLock()
		entry := whitelist[a.bin]
		mu.RUnlock()
		if err := validateArgs(entry, a.args); err != nil {
			t.Errorf("expected allow: %s %v: %v", a.bin, a.args, err)
		}
	}
}
