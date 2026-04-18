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

func TestRegisterDefaultsValidation(t *testing.T) {
	resetWhitelist()
	RegisterDefaults()

	// Spot-check a few injection attempts panic.
	cases := []struct {
		name string
		bin  string
		args []string
	}{
		{"journal_unit_inj", "/bin/journalctl", []string{"-u", "kubelet.service; rm -rf /", "--since", "1 hour ago", "-n", "10", "-o", "json", "--no-pager"}},
		{"journal_format_inj", "/bin/journalctl", []string{"-u", "kubelet.service", "--since", "1 hour ago", "-n", "10", "-o", "csv", "--no-pager"}},
		{"systemctl_extra", "/bin/systemctl", []string{"reboot"}},
		{"ss_unknown_flag", "/usr/sbin/ss", []string{"-K"}},
		{"unknown_bin", "/bin/rm", []string{"-rf", "/"}},
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
		{"/bin/journalctl", []string{"-u", "kubelet.service", "--since", "1 hour ago", "-n", "100", "-o", "json", "--no-pager"}},
		{"/bin/systemctl", []string{"list-units", "--all", "--no-pager", "--no-legend", "-o", "json"}},
		{"/usr/sbin/ss", []string{"-tulpn"}},
		{"/sbin/ip", []string{"-json", "addr"}},
		{"/sbin/iptables", []string{"-L", "-n", "-v"}},
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
