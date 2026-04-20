// Package release polls the GitHub Releases API for the latest agent/hub
// release tag and caches it for the web UI and the agent-self-update flow.
//
// Read-only: this package never modifies the agent or the host. It only
// exposes "what's the newest published tag" so the UI can show an outdated
// badge and the agent's own self-updater (opt-in) can decide whether to
// replace its binary.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vasyakrg/recon/internal/common/version"
)

var outdated = version.Outdated

type Poller struct {
	apiURL   string // https://api.github.com/repos/<owner>/<repo>/releases/latest
	htmlURL  string // https://github.com/<owner>/<repo>/releases
	interval time.Duration
	http     *http.Client
	log      *slog.Logger

	mu        sync.RWMutex
	latest    string    // e.g. "v0.1.4" — empty until first successful poll
	fetchedAt time.Time // last successful poll
	lastErr   error
	lastErrAt time.Time
	apiRate   string // github rate-limit hint from response headers
}

type Info struct {
	Latest    string
	FetchedAt time.Time
	LastErr   string
	APIRate   string
}

// New constructs a Poller from a repo URL like
// "https://github.com/vasyakrg/reconops". Interval <=0 falls back to 30m.
// Returns nil if repoURL can't be parsed — the caller should treat nil as
// "release info unavailable" rather than failing hub startup.
func New(repoURL string, interval time.Duration, log *slog.Logger) *Poller {
	owner, repo, ok := parseRepo(repoURL)
	if !ok {
		return nil
	}
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	return &Poller{
		apiURL:   fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo),
		htmlURL:  fmt.Sprintf("https://github.com/%s/%s/releases", owner, repo),
		interval: interval,
		http:     &http.Client{Timeout: 10 * time.Second},
		log:      log,
	}
}

// Run blocks until ctx is done. First poll fires immediately; subsequent
// polls fire on interval. Transient errors are logged + surfaced via Info().
func (p *Poller) Run(ctx context.Context) {
	if p == nil {
		return
	}
	p.pollOnce(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollOnce(ctx)
		}
	}
}

// Latest returns the cached tag (e.g. "v0.1.4") and ok=true if a successful
// poll has happened at least once. Safe to call from any goroutine.
func (p *Poller) Latest() (string, bool) {
	if p == nil {
		return "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.latest, p.latest != ""
}

// ReleasesURL is the public releases page on github.com — rendered as a
// link in the UI so operators can open the changelog with one click.
func (p *Poller) ReleasesURL() string {
	if p == nil {
		return ""
	}
	return p.htmlURL
}

func (p *Poller) Info() Info {
	if p == nil {
		return Info{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := Info{Latest: p.latest, FetchedAt: p.fetchedAt, APIRate: p.apiRate}
	if p.lastErr != nil {
		out.LastErr = p.lastErr.Error()
	}
	return out
}

func (p *Poller) pollOnce(ctx context.Context) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.apiURL, nil)
	if err != nil {
		p.recordErr(err)
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "recon-hub-release-poller")
	resp, err := p.http.Do(req)
	if err != nil {
		p.recordErr(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	rate := resp.Header.Get("X-RateLimit-Remaining")
	if resp.StatusCode == http.StatusNotFound {
		// No releases yet — fine, not an error we want to spam the log with.
		p.mu.Lock()
		p.lastErr = errors.New("no releases published yet")
		p.lastErrAt = time.Now().UTC()
		p.apiRate = rate
		p.mu.Unlock()
		return
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		p.recordErr(fmt.Errorf("github api %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}
	var payload struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
		Pre     bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		p.recordErr(err)
		return
	}
	if payload.TagName == "" {
		p.recordErr(errors.New("empty tag_name"))
		return
	}
	p.mu.Lock()
	p.latest = payload.TagName
	p.fetchedAt = time.Now().UTC()
	p.lastErr = nil
	p.apiRate = rate
	p.mu.Unlock()
	if p.log != nil {
		p.log.Debug("release poll ok", "tag", payload.TagName, "rate_remaining", rate)
	}
}

func (p *Poller) recordErr(err error) {
	p.mu.Lock()
	p.lastErr = err
	p.lastErrAt = time.Now().UTC()
	p.mu.Unlock()
	if p.log != nil {
		p.log.Warn("release poll failed", "err", err)
	}
}

// parseRepo accepts "https://github.com/owner/repo[.git][/...]" and returns
// (owner, repo). Anything else returns ok=false.
func parseRepo(u string) (string, string, bool) {
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, "/")
	const prefix = "https://github.com/"
	if !strings.HasPrefix(u, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(u, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	return parts[0], repo, true
}

// Outdated re-exports version.Outdated so existing callers (web template
// FuncMap) keep working through this package.
func Outdated(current, latest string) bool {
	return outdated(current, latest)
}
