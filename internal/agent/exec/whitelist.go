package exec

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Argument validators used across whitelist entries.

// NoShellMeta rejects any shell-special character that could allow injection
// even though we never invoke a shell — defence in depth in case some future
// caller passes the slice through `sh -c`.
func NoShellMeta(s string) error {
	if strings.ContainsAny(s, ";|&`$<>\n\r\t\x00*?") {
		return errors.New("shell metacharacters not allowed")
	}
	if strings.Contains(s, "..") {
		return errors.New("path traversal not allowed")
	}
	return nil
}

// SystemdUnitName matches the typical systemd unit name shape; rejects shell
// metacharacters and slashes.
var unitRe = regexp.MustCompile(`^[A-Za-z0-9@:._\-]{1,256}$`)

func SystemdUnitName(s string) error {
	if !unitRe.MatchString(s) {
		return fmt.Errorf("invalid systemd unit name: %q", s)
	}
	return nil
}

// PosInt requires the value to parse as a positive int within [1, max].
func PosInt(max int) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("not an int: %w", err)
		}
		if n < 1 || n > max {
			return fmt.Errorf("out of range [1,%d]: %d", max, n)
		}
		return nil
	}
}

// JournalSince accepts values journalctl(1) understands without metacharacters.
// Either an ISO-ish timestamp ("2026-04-18 09:00:00"), a human form
// ("1 hour ago", "yesterday"), or a relative duration ("-15m"). We restrict
// to a conservative regex.
var journalSinceRe = regexp.MustCompile(`^[A-Za-z0-9 :.\-+_]{1,64}$`)

func JournalSince(s string) error {
	if !journalSinceRe.MatchString(s) {
		return fmt.Errorf("invalid since value: %q", s)
	}
	return nil
}

// HostPort accepts host:port with conservative chars only; used by
// connectivity checks.
var hostPortRe = regexp.MustCompile(`^[A-Za-z0-9._\-]{1,253}:[0-9]{1,5}$`)

func HostPort(s string) error {
	if !hostPortRe.MatchString(s) {
		return fmt.Errorf("invalid host:port: %q", s)
	}
	return nil
}

// DurationLike accepts strings parsable by time.ParseDuration up to 10m.
func DurationLike(max time.Duration) func(string) error {
	return func(s string) error {
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		if d <= 0 || d > max {
			return fmt.Errorf("duration %s out of (0, %s]", d, max)
		}
		return nil
	}
}

// RegisterDefaults populates the read-only whitelist with the entries needed
// by the first wave of collectors. Call from agent main, before starting
// the conn loop. Idempotent only on first call — re-Register panics.
func RegisterDefaults() {
	// systemctl list-units (used by systemd_units collector).
	Register(Entry{
		Bin:     "/bin/systemctl",
		Timeout: 10 * time.Second,
		Patterns: [][]ArgSpec{
			// systemctl list-units --all --no-pager --no-legend -o json
			{
				{Value: literal("list-units")},
				{Value: literal("--all")},
				{Value: literal("--no-pager")},
				{Value: literal("--no-legend")},
				{Value: literal("-o")}, {Value: literal("json")},
			},
			// systemctl status <unit> --no-pager (read-only)
			{
				{Value: literal("status")},
				{Value: SystemdUnitName},
				{Value: literal("--no-pager")},
			},
		},
	})

	// journalctl — used by journal_tail.
	Register(Entry{
		Bin:     "/bin/journalctl",
		Timeout: 30 * time.Second,
		Patterns: [][]ArgSpec{
			// journalctl -u <unit> --since <ts> -n <N> -o json --no-pager
			{
				{Flag: "-u"}, {Value: SystemdUnitName},
				{Flag: "--since"}, {Value: JournalSince},
				{Flag: "-n"}, {Value: PosInt(100000)},
				{Flag: "-o"}, {Value: literal("json")},
				{Value: literal("--no-pager")},
			},
		},
	})

	// ss — listen sockets.
	Register(Entry{
		Bin:     "/usr/sbin/ss",
		Timeout: 5 * time.Second,
		Patterns: [][]ArgSpec{
			{{Value: literal("-tulpn")}},
			{{Value: literal("-tan")}},
			{{Value: literal("-uan")}},
		},
	})

	// ip — interfaces / routes / neighbours.
	Register(Entry{
		Bin:     "/sbin/ip",
		Timeout: 5 * time.Second,
		Patterns: [][]ArgSpec{
			{{Value: literal("-json")}, {Value: literal("addr")}},
			{{Value: literal("-json")}, {Value: literal("link")}},
			{{Value: literal("-json")}, {Value: literal("route")}},
			{{Value: literal("-json")}, {Value: literal("neigh")}},
		},
	})

	// iptables -L -n -v (read-only listing only).
	Register(Entry{
		Bin:     "/sbin/iptables",
		Timeout: 10 * time.Second,
		Patterns: [][]ArgSpec{
			{
				{Value: literal("-L")},
				{Value: literal("-n")},
				{Value: literal("-v")},
			},
		},
	})
}

func literal(want any) func(string) error {
	switch v := want.(type) {
	case string:
		return func(s string) error {
			if s != v {
				return fmt.Errorf("expected literal %q, got %q", v, s)
			}
			return nil
		}
	case func(string) error:
		return v
	default:
		panic("literal: unsupported type")
	}
}
