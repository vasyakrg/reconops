package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		raw   string
		want  slog.Level
		valid bool
	}{
		{"", slog.LevelInfo, true},
		{"info", slog.LevelInfo, true},
		{"INFO", slog.LevelInfo, true},
		{"  debug  ", slog.LevelDebug, true},
		{"Debug", slog.LevelDebug, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"trace", slog.LevelInfo, false}, // unknown → info, flagged invalid
		{"verbose", slog.LevelInfo, false},
	}
	for _, c := range cases {
		got, ok := ParseLevel(c.raw)
		if got != c.want || ok != c.valid {
			t.Errorf("ParseLevel(%q) = (%v,%v), want (%v,%v)", c.raw, got, ok, c.want, c.valid)
		}
	}
}

func TestLevelString(t *testing.T) {
	for level, want := range map[slog.Level]string{
		slog.LevelDebug: "debug",
		slog.LevelInfo:  "info",
		slog.LevelWarn:  "warn",
		slog.LevelError: "error",
	} {
		if got := LevelString(level); got != want {
			t.Errorf("LevelString(%v) = %q, want %q", level, got, want)
		}
	}
	// Round-trips with ParseLevel for the canonical tokens.
	for _, tok := range []string{"debug", "info", "warn", "error"} {
		lvl, ok := ParseLevel(tok)
		if !ok || LevelString(lvl) != tok {
			t.Errorf("round-trip failed for %q (lvl=%v ok=%v)", tok, lvl, ok)
		}
	}
}
