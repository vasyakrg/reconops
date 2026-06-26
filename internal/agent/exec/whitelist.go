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

// JournalGrep validates a journalctl -g pattern. It deliberately PERMITS regex
// metacharacters (the whole point of -g) but blocks control characters, and
// requires the pattern to compile as an RE2 regex — a conservative subset of
// journald's PCRE that rejects pathological/garbage input. Do NOT reuse
// NoShellMeta here: it bans * ? | which are legal regex.
func JournalGrep(s string) error {
	if s == "" {
		return errors.New("empty grep pattern")
	}
	if len(s) > 512 {
		return fmt.Errorf("grep pattern too long (%d bytes); max 512", len(s))
	}
	if strings.ContainsAny(s, "\x00\n\r") {
		return errors.New("control characters not allowed in grep pattern")
	}
	if _, err := regexp.Compile(s); err != nil {
		return fmt.Errorf("invalid regex: %w", err)
	}
	return nil
}

var priorityNames = map[string]bool{
	"emerg": true, "alert": true, "crit": true, "err": true,
	"warning": true, "notice": true, "info": true, "debug": true,
}

// PriorityLevel validates a journalctl -p value: a single level (0-7 or a name
// like "err") or a range ("0..3" / "warning..debug").
func PriorityLevel(s string) error {
	parts := strings.SplitN(s, "..", 2)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return fmt.Errorf("invalid priority %q", s)
		}
		if n, err := strconv.Atoi(p); err == nil {
			if n < 0 || n > 7 {
				return fmt.Errorf("priority out of range [0,7]: %d", n)
			}
			continue
		}
		if !priorityNames[strings.ToLower(p)] {
			return fmt.Errorf("invalid priority token %q", p)
		}
	}
	return nil
}

// journalctlPatterns enumerates the allowed journalctl arg shapes. The collector
// emits flags in a fixed canonical order — selector, boot, --since, --until, -p,
// -g, then -n -o json --no-pager — and this generates the cartesian product of
// the optional segments so validateArgs has an exact positional match for every
// combination the collector can produce. -o json is on EVERY shape so the
// summarizeJournal parser always has JSON to read.
func journalctlPatterns() [][]ArgSpec {
	tail := []ArgSpec{
		{Flag: "-n"}, {Value: PosInt(100000)},
		{Flag: "-o"}, {Value: literal("json")},
		{Value: literal("--no-pager")},
	}
	selectors := [][]ArgSpec{
		{},                                       // whole journal
		{{Flag: "-u"}, {Value: SystemdUnitName}}, // one unit
		{{Flag: "-k"}},                           // kernel ring (--dmesg)
	}
	boots := [][]ArgSpec{{}, {{Flag: "-b"}, {Value: literal("-1")}}}
	sinces := [][]ArgSpec{{}, {{Flag: "--since"}, {Value: JournalSince}}}
	untils := [][]ArgSpec{{}, {{Flag: "--until"}, {Value: JournalSince}}}
	prios := [][]ArgSpec{{}, {{Flag: "-p"}, {Value: PriorityLevel}}}
	greps := [][]ArgSpec{{}, {{Flag: "-g"}, {Value: JournalGrep}}}

	var pats [][]ArgSpec
	for _, sel := range selectors {
		for _, boot := range boots {
			for _, since := range sinces {
				for _, until := range untils {
					for _, prio := range prios {
						for _, grep := range greps {
							p := make([]ArgSpec, 0, 16)
							p = append(p, sel...)
							p = append(p, boot...)
							p = append(p, since...)
							p = append(p, until...)
							p = append(p, prio...)
							p = append(p, grep...)
							p = append(p, tail...)
							pats = append(pats, p)
						}
					}
				}
			}
		}
	}
	return pats
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
		Bin:        "systemctl",
		Candidates: []string{"/bin/systemctl", "/usr/bin/systemctl", "/usr/sbin/systemctl"},
		Timeout:    10 * time.Second,
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

	// journalctl — used by journal_tail. Hard cap stdout at 16 MiB so a
	// runaway --since/--lines combo cannot OOM the agent (review H2).
	// Patterns are generated to cover optional unit / -k / -b -1 / --until /
	// -p / -g in a fixed canonical order (see journalctlPatterns).
	Register(Entry{
		Bin:            "journalctl",
		Candidates:     []string{"/bin/journalctl", "/usr/bin/journalctl", "/usr/sbin/journalctl"},
		Timeout:        30 * time.Second,
		MaxStdoutBytes: 16 * 1024 * 1024,
		Patterns:       journalctlPatterns(),
	})

	// dmesg — kernel ring buffer with absolute ISO timestamps. Grep is done
	// in-process by the collector (Go RE2), so the only allowed arg shape is the
	// fixed --time-format=iso. Reading the ring needs CAP_SYSLOG on hosts with
	// kernel.dmesg_restrict=1.
	Register(Entry{
		Bin:            "dmesg",
		Candidates:     []string{"/bin/dmesg", "/usr/bin/dmesg", "/sbin/dmesg"},
		Timeout:        10 * time.Second,
		MaxStdoutBytes: 16 * 1024 * 1024,
		Patterns: [][]ArgSpec{
			{{Value: literal("--time-format=iso")}},
		},
	})

	// ss — listen sockets. -p adds process info (needs root or CAP_NET_ADMIN);
	// agent runs with NET_ADMIN so it gets PID/comm of socket owners.
	Register(Entry{
		Bin:        "ss",
		Candidates: []string{"/usr/sbin/ss", "/usr/bin/ss", "/sbin/ss", "/bin/ss"},
		Timeout:    5 * time.Second,
		Patterns: [][]ArgSpec{
			{{Value: literal("-tulpn")}},
			{{Value: literal("-tan")}},
			{{Value: literal("-uan")}},
		},
	})

	// ip — interfaces / routes / neighbours.
	Register(Entry{
		Bin:        "ip",
		Candidates: []string{"/sbin/ip", "/usr/sbin/ip", "/usr/bin/ip", "/bin/ip"},
		Timeout:    5 * time.Second,
		Patterns: [][]ArgSpec{
			{{Value: literal("-json")}, {Value: literal("addr")}},
			{{Value: literal("-json")}, {Value: literal("link")}},
			{{Value: literal("-json")}, {Value: literal("route")}},
			{{Value: literal("-json")}, {Value: literal("neigh")}},
		},
	})

	// iptables -L -n -v (read-only listing only).
	Register(Entry{
		Bin:        "iptables",
		Candidates: []string{"/sbin/iptables", "/usr/sbin/iptables", "/usr/bin/iptables", "/bin/iptables"},
		Timeout:    10 * time.Second,
		Patterns: [][]ArgSpec{
			{
				{Value: literal("-L")},
				{Value: literal("-n")},
				{Value: literal("-v")},
			},
		},
	})

	// docker — read-only inspection only. Listing, single-container detail,
	// and bounded log tail. No --no-trunc on ps/inspect because the JSON
	// output is fixed-format anyway. CLI never passed user-controlled
	// metacharacters; container_id / name validated as "no shell meta".
	Register(Entry{
		Bin:            "docker",
		Candidates:     []string{"/usr/bin/docker", "/usr/local/bin/docker", "/snap/bin/docker"},
		Timeout:        15 * time.Second,
		MaxStdoutBytes: 8 * 1024 * 1024,
		Patterns: [][]ArgSpec{
			// docker ps -a --no-trunc --format '{{json .}}'
			{
				{Value: literal("ps")}, {Value: literal("-a")},
				{Value: literal("--no-trunc")},
				{Value: literal("--format")}, {Value: literal("{{json .}}")},
			},
			// docker inspect <id-or-name>
			{
				{Value: literal("inspect")}, {Value: NoShellMeta},
			},
			// docker logs --tail <N> --timestamps <id-or-name>
			{
				{Value: literal("logs")},
				{Value: literal("--tail")}, {Value: PosInt(10000)},
				{Value: literal("--timestamps")},
				{Value: NoShellMeta},
			},
		},
	})

	// kubectl — read-only resource listing + describe + log tail. The
	// agent uses the host's kubeconfig (typically /root/.kube/config or
	// /etc/kubernetes/admin.conf, mounted on the agent host); no creds
	// land in the agent itself.
	Register(Entry{
		Bin:            "kubectl",
		Candidates:     []string{"/usr/bin/kubectl", "/usr/local/bin/kubectl", "/snap/bin/kubectl"},
		Timeout:        20 * time.Second,
		MaxStdoutBytes: 16 * 1024 * 1024,
		Patterns: [][]ArgSpec{
			// kubectl get <resource> -A -o json
			{
				{Value: literal("get")}, {Value: NoShellMeta},
				{Value: literal("-A")},
				{Value: literal("-o")}, {Value: literal("json")},
			},
			// kubectl get <resource> -n <ns> -o json
			{
				{Value: literal("get")}, {Value: NoShellMeta},
				{Value: literal("-n")}, {Value: NoShellMeta},
				{Value: literal("-o")}, {Value: literal("json")},
			},
			// kubectl describe <resource> <name> -n <ns>
			{
				{Value: literal("describe")}, {Value: NoShellMeta}, {Value: NoShellMeta},
				{Value: literal("-n")}, {Value: NoShellMeta},
			},
			// kubectl logs <pod> -n <ns> --tail <N> --timestamps
			{
				{Value: literal("logs")}, {Value: NoShellMeta},
				{Value: literal("-n")}, {Value: NoShellMeta},
				{Value: literal("--tail")}, {Value: PosInt(10000)},
				{Value: literal("--timestamps")},
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
