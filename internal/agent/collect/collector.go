// Package collect defines the Collector contract that every read-only probe
// must satisfy and the registry through which the agent enumerates them.
//
// Collectors are compiled into the agent binary — there is no plugin loader.
// New collector = new agent release. This is layer 2 of the read-only
// guarantee (PROJECT.md §3.4).
package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Capability is a coarse-grained privilege requirement declared in a
// Manifest. The agent process is responsible for ensuring it actually has
// the listed capability before invoking the collector.
type Capability string

const (
	CapSudoJournalctl Capability = "SUDO_JOURNALCTL"
	CapSudoSS         Capability = "SUDO_SS"
	CapSudoIptables   Capability = "SUDO_IPTABLES"
	CapDACReadSearch  Capability = "CAP_DAC_READ_SEARCH"
	CapNetRaw         Capability = "CAP_NET_RAW"
)

type ParamSpec struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // string|int|duration|bool
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description"`
}

type Manifest struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Category     string       `json:"category"`
	Description  string       `json:"description"`
	Reads        []string     `json:"reads"`
	Requires     []Capability `json:"requires,omitempty"`
	ParamsSchema []ParamSpec  `json:"params_schema,omitempty"`
	OutputSample any          `json:"output_sample,omitempty"`
}

type Hint struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Evidence any    `json:"evidence,omitempty"`
}

type Artifact struct {
	Name string
	Mime string
	Body []byte // small enough to fit in memory; large ones use streaming separately
}

type Result struct {
	Data      any
	Artifacts []Artifact
	Hints     []Hint
	Stderr    string
	ExitCode  int
}

// Params is the parameter map for a single Run. Stringly-typed because the
// gRPC contract uses map<string,string>; collectors parse to the right type.
type Params map[string]string

type Collector interface {
	Manifest() Manifest
	Run(ctx context.Context, p Params) (Result, error)
}

// Availabler is an optional interface a collector can implement to gate
// itself on host capabilities (binary present, daemon socket reachable,
// /proc entry exists, etc). Collectors that don't implement it are always
// available — no implicit gate.
//
// The agent calls Available() exactly once, at startup right after every
// init() has populated the registry. Collectors returning false are
// permanently unregistered for the lifetime of this agent process — they
// never appear in the Hello manifest list, so the LLM investigator never
// sees them as candidates and can't waste a probe step.
//
// Re-checks on a long-lived agent are out of scope: if docker is installed
// after the agent started, restart the agent. The alternative (poll-and-
// re-advertise) adds non-trivial protocol churn for a rare ops event.
type Availabler interface {
	Available() bool
}

// AvailabilityDiff is the result of one RefreshAvailability tick: which
// collectors transitioned from unavailable→available since the last call
// (NowAvailable) and which transitioned the other way (NowUnavailable).
// Empty fields mean the visible manifest set is unchanged.
type AvailabilityDiff struct {
	NowAvailable   []string // sorted
	NowUnavailable []string // sorted
}

// Changed reports whether the most recent refresh altered the visible
// manifest set. Convenience wrapper for the agent's manifest-resync loop.
func (d AvailabilityDiff) Changed() bool {
	return len(d.NowAvailable) > 0 || len(d.NowUnavailable) > 0
}

// RefreshAvailability re-runs Available() on every Availabler in the
// registry and updates the cached state. Returns the diff vs. the previous
// snapshot so the caller can decide whether to re-advertise the manifest
// list (e.g. send a fresh Hello to the hub when docker was just installed
// or removed).
//
// Collectors that don't implement Availabler are not touched — they're
// always visible. First call after process start treats every Availabler
// as transitioning from "previously unknown" to its current state, which
// gives the agent's startup log a clean "added/dropped at boot" record.
//
// Safe to call from any goroutine; takes the registry mutex for write.
func RefreshAvailability() AvailabilityDiff {
	mu.Lock()
	defer mu.Unlock()
	var added, removed []string
	first := len(available) == 0
	for name, c := range registry {
		a, ok := c.(Availabler)
		if !ok {
			continue
		}
		now := a.Available()
		prev, had := available[name]
		available[name] = now
		switch {
		case first:
			// Treat first-ever probe as a transition from "unset" — surface
			// only the dropped ones so the boot log is "registered but
			// unavailable: …". The added set is the rest of the registry,
			// which the operator can read from /collectors anyway.
			if !now {
				removed = append(removed, name)
			}
		case had && prev != now:
			if now {
				added = append(added, name)
			} else {
				removed = append(removed, name)
			}
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return AvailabilityDiff{NowAvailable: added, NowUnavailable: removed}
}

var (
	mu       sync.RWMutex
	registry = map[string]Collector{}
	// available tracks the last RefreshAvailability() result. Collectors
	// without an Availabler implementation are implicitly "true" and never
	// appear here. Manifests() / Get() consult this map to hide collectors
	// whose host capability went away (e.g. docker uninstalled at runtime).
	available = map[string]bool{}
)

// Register adds c to the registry. Called from init() in concrete collector
// packages. Duplicate names panic — that is a programming error.
func Register(c Collector) {
	m := c.Manifest()
	if m.Name == "" {
		panic("collect: empty manifest name")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[m.Name]; dup {
		panic(fmt.Sprintf("collect: duplicate collector %q", m.Name))
	}
	registry[m.Name] = c
}

// Get returns a registered collector iff it is also currently available
// per the most recent RefreshAvailability() snapshot. A collector that
// implements Availabler but reported false is hidden — Get returns ok=false
// even though it lives in the registry. This protects the runner from
// executing a probe whose host capability went away since startup.
func Get(name string) (Collector, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[name]
	if !ok {
		return nil, false
	}
	if v, gated := available[name]; gated && !v {
		return nil, false
	}
	return c, true
}

// All returns only the currently-available collectors. See Get for the
// availability semantics.
func All() []Collector {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Collector, 0, len(registry))
	for name, c := range registry {
		if v, gated := available[name]; gated && !v {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Manifests returns the manifest list the agent should advertise to the
// hub on Hello — only currently-available collectors. Names sorted for
// deterministic ordering, which makes Hello payloads diffable in logs.
func Manifests() []Manifest {
	cs := All()
	out := make([]Manifest, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Manifest())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m Manifest) JSON() ([]byte, error) { return json.Marshal(m) }

// UnregisterForTest removes a collector from the registry. Test-only —
// production code never unregisters compiled-in collectors (PROJECT.md §3.4
// layer 2). Safe to call for an unknown name.
func UnregisterForTest(name string) {
	mu.Lock()
	defer mu.Unlock()
	delete(registry, name)
}
