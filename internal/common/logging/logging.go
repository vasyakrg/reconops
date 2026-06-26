// Package logging builds the process-wide structured logger from environment
// configuration. Both binaries (hub, agent) previously hardcoded slog.LevelInfo,
// so the Debug trail (e.g. the [FIX:auto-approve] decision log) was invisible on
// prod with no way to raise verbosity without a code change. RECON_LOG_LEVEL is
// the single, external knob (CLAUDE.md: all settings external — never hardcode).
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// EnvLogLevel is the environment variable controlling log verbosity for both
// the hub and the agent.
const EnvLogLevel = "RECON_LOG_LEVEL"

// ParseLevel resolves a slog.Level from a RECON_LOG_LEVEL value
// (debug|info|warn|error, case-insensitive; empty → info). The bool reports
// whether the value was recognized, so callers can warn on a typo while still
// starting up at the safe info default.
func ParseLevel(raw string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// New builds the stderr JSON logger at the RECON_LOG_LEVEL level (default info)
// and returns the resolved level so the caller can announce it in its startup
// line. Debug on prod is safe — the Debug sites carry no secret/PII.
func New() (*slog.Logger, slog.Level) {
	level, _ := ParseLevel(os.Getenv(EnvLogLevel))
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	return logger, level
}

// LevelString renders a level as a lowercase token (debug|info|warn|error) for
// structured log fields, matching the accepted RECON_LOG_LEVEL spellings.
func LevelString(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return "info"
	}
}
