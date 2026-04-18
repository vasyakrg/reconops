package systemd

import (
	"strings"
	"testing"
)

// FuzzParseUnits / FuzzSummarizeJournal — must not panic on arbitrary input.
func FuzzParseUnits(f *testing.F) {
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"unit":"x","load":"loaded","active":"active","sub":"r","description":"y"}]`))
	f.Add([]byte(`{"truncated"`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = parseUnits(body)
	})
}

func FuzzSummarizeJournal(f *testing.F) {
	f.Add([]byte(`{"PRIORITY":"3","MESSAGE":"x","_SYSTEMD_UNIT":"u","__REALTIME_TIMESTAMP":"1"}`))
	f.Add([]byte(""))
	f.Add([]byte(`{"PRIORITY":"4"`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = summarizeJournal("u", "1h", body)
	})
}

func TestParseUnits(t *testing.T) {
	body := []byte(`[
{"unit":"sshd.service","load":"loaded","active":"active","sub":"running","description":"OpenSSH server"},
{"unit":"foo.service","load":"loaded","active":"failed","sub":"failed","description":"Broken"},
{"unit":"bar.service","load":"loaded","active":"inactive","sub":"dead","description":"Stopped"}
]`)
	out, hints := parseUnits(body)
	if out.Total != 3 || out.Failed != 1 || out.Inactive != 1 {
		t.Fatalf("totals: %+v", out)
	}
	if len(hints) != 1 || hints[0].Code != "service.failed" {
		t.Fatalf("hints: %+v", hints)
	}
}

func TestSummarizeJournal(t *testing.T) {
	body := []byte(`{"PRIORITY":"3","MESSAGE":"oops","_SYSTEMD_UNIT":"foo.service","__REALTIME_TIMESTAMP":"1"}
{"PRIORITY":"4","MESSAGE":"warn","_SYSTEMD_UNIT":"foo.service","__REALTIME_TIMESTAMP":"2"}
{"PRIORITY":"6","MESSAGE":"info","_SYSTEMD_UNIT":"foo.service","__REALTIME_TIMESTAMP":"3"}
{"PRIORITY":"3","MESSAGE":"another error","_SYSTEMD_UNIT":"foo.service","__REALTIME_TIMESTAMP":"4"}
`)
	out, hints := summarizeJournal("foo.service", "1 hour ago", body)
	if out.Lines != 4 || out.Errors != 2 || out.Warnings != 1 {
		t.Fatalf("counts: %+v", out)
	}
	if out.Levels["3"] != 2 || out.Levels["4"] != 1 || out.Levels["6"] != 1 {
		t.Fatalf("levels: %+v", out.Levels)
	}
	if len(hints) != 2 {
		t.Fatalf("hints: %d", len(hints))
	}
}

func TestSanitizeUnit(t *testing.T) {
	if got := sanitizeUnit("foo/bar.service"); got != "foo_bar.service" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeUnit("getty@tty1.service"); got != "getty_tty1.service" {
		t.Fatalf("got %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Fatal("no truncate")
	}
	if !strings.HasSuffix(truncate("hello world", 5), "…") {
		t.Fatal("missing ellipsis")
	}
}
