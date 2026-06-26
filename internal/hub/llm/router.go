package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Operation names the investigator passes to the router so it can pick a
// model profile by task (Task 13).
const (
	OpPlanNextStep     = "plan_next_step"
	OpCompactMemory    = "compact_memory"
	OpLogTriageSummary = "log_triage_summary"
	OpVerifyFinding    = "verify_finding"
	OpFinalSummary     = "final_summary"
)

// Profile is the routing-facing view of a configured model profile. The hub
// config (package main) maps its LLMProfileConfig onto this so the llm package
// does not import package main.
type Profile struct {
	Name                string
	Role                string
	Model               string
	BaseURL             string
	APIKeyEnv           string
	ContextWindowTokens int
	MaxOutputTokens     int
	SupportsTools       bool
	SupportsPromptCache bool
	AllowInsecureHTTP   bool
	HTTPReferer         string
	XTitle              string
}

type routedProfile struct {
	Profile
	client *Client
}

// Router selects a model profile per investigator operation and owns the
// per-operation retry + cross-profile fallback policy (Task 13). Built once at
// startup from the resolved profiles.
type Router struct {
	order   []*routedProfile
	byRole  map[string]*routedProfile
	primary *routedProfile
}

// Selection is the resolved profile for one operation.
type Selection struct {
	Profile             string
	Model               string
	ContextWindowTokens int
	MaxOutputTokens     int
	SupportsPromptCache bool
	client              *Client
}

// NewRouter builds one Client per profile. A primary profile is required (the
// first profile with role "primary", else the first profile). Profiles whose
// API key env is unset fail here so startup can report which profile is
// misconfigured.
func NewRouter(profiles []Profile) (*Router, error) {
	if len(profiles) == 0 {
		return nil, errors.New("llm router: no profiles configured")
	}
	r := &Router{byRole: map[string]*routedProfile{}}
	for _, p := range profiles {
		c, err := NewFromEnv(p.BaseURL, p.Model, p.APIKeyEnv, p.AllowInsecureHTTP, p.HTTPReferer, p.XTitle)
		if err != nil {
			return nil, fmt.Errorf("llm router: profile %q: %w", p.Name, err)
		}
		rp := &routedProfile{Profile: p, client: c}
		r.order = append(r.order, rp)
		if _, ok := r.byRole[p.Role]; !ok {
			r.byRole[p.Role] = rp
		}
		if p.Role == "primary" && r.primary == nil {
			r.primary = rp
		}
	}
	if r.primary == nil {
		r.primary = r.order[0]
	}
	return r, nil
}

// Primary returns the primary client (back-compat for callers that only need a
// single model: Info(), budgets, etc.).
func (r *Router) Primary() *Client { return r.primary.client }

// Profiles returns the configured profile names in order, for diagnostics.
func (r *Router) Profiles() []Profile {
	out := make([]Profile, 0, len(r.order))
	for _, rp := range r.order {
		out = append(out, rp.Profile)
	}
	return out
}

func roleForOperation(op string) string {
	switch op {
	case OpCompactMemory:
		return "summarizer"
	case OpLogTriageSummary:
		return "cheap"
	case OpVerifyFinding, OpFinalSummary:
		return "verifier"
	default: // OpPlanNextStep and any tool-calling turn
		return "primary"
	}
}

func (r *Router) profileFor(operation string, requireTools bool, forced string) *routedProfile {
	if forced != "" {
		for _, rp := range r.order {
			if rp.Name == forced && (!requireTools || rp.SupportsTools) {
				return rp
			}
		}
	}
	role := roleForOperation(operation)
	if rp := r.byRole[role]; rp != nil && (!requireTools || rp.SupportsTools) {
		return rp
	}
	return r.primary
}

func selectionOf(rp *routedProfile) Selection {
	return Selection{
		Profile:             rp.Name,
		Model:               rp.Model,
		ContextWindowTokens: rp.ContextWindowTokens,
		MaxOutputTokens:     rp.MaxOutputTokens,
		SupportsPromptCache: rp.SupportsPromptCache,
		client:              rp.client,
	}
}

// Select resolves (without calling) the profile for an operation, honoring an
// optional forced profile name (per-investigation override, Task 14) and a
// tool-support requirement.
func (r *Router) Select(operation string, requireTools bool, forced string) Selection {
	return selectionOf(r.profileFor(operation, requireTools, forced))
}

const maxRetries = 2

// Chat runs req against the operation's profile with retry + fallback:
//   - transient errors (429/5xx/network) retry the same profile up to
//     maxRetries with capped backoff;
//   - if it still fails on a fallback-eligible class and a different,
//     tool-compatible profile (the primary) exists, try that once;
//   - otherwise the last typed error is returned.
//
// The returned Selection is the profile that actually produced the response
// (or the last one tried). fallback reports how many times the router switched
// profiles (0 or 1).
func (r *Router) Chat(ctx context.Context, operation string, requireTools bool, forced string, req ChatRequest) (resp *ChatResponse, sel Selection, fallback int, err error) {
	primary := r.profileFor(operation, requireTools, forced)
	resp, err = r.chatWithRetry(ctx, primary, req)
	if err == nil {
		return resp, selectionOf(primary), 0, nil
	}
	if shouldFallback(err) {
		fb := r.primary
		if fb != primary && (!requireTools || fb.SupportsTools) {
			if resp2, err2 := r.chatWithRetry(ctx, fb, req); err2 == nil {
				return resp2, selectionOf(fb), 1, nil
			}
		}
	}
	return nil, selectionOf(primary), 0, err
}

func (r *Router) chatWithRetry(ctx context.Context, rp *routedProfile, req ChatRequest) (*ChatResponse, error) {
	req.Model = rp.Model
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := rp.client.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var pe *ProviderError
		if !errors.As(err, &pe) || !pe.Temporary() {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return nil, lastErr
}

func backoff(attempt int) time.Duration {
	d := time.Duration(500*(1<<attempt)) * time.Millisecond // 0.5s, 1s, 2s
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func shouldFallback(err error) bool {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Class {
	case ProviderErrorServer, ProviderErrorNetwork, ProviderErrorRateLimit:
		return true
	default:
		return false
	}
}
