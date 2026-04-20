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

// PruneUnavailable walks the registry, calls Available() on every
// collector that implements Availabler, and removes those returning false.
// Returns the names that were dropped (sorted, for stable logging).
// Idempotent — safe to call multiple times. Intended to be called once
// from cmd/agent/main.go after exec.RegisterDefaults().
func PruneUnavailable() []string {
	mu.Lock()
	defer mu.Unlock()
	dropped := make([]string, 0)
	for name, c := range registry {
		a, ok := c.(Availabler)
		if !ok {
			continue
		}
		if !a.Available() {
			delete(registry, name)
			dropped = append(dropped, name)
		}
	}
	sort.Strings(dropped)
	return dropped
}

var (
	mu       sync.RWMutex
	registry = map[string]Collector{}
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

func Get(name string) (Collector, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[name]
	return c, ok
}

func All() []Collector {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Collector, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	return out
}

func Manifests() []Manifest {
	cs := All()
	out := make([]Manifest, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Manifest())
	}
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
