// Package web serves the operator UI. Week 2 ships pages for hosts inventory,
// collector catalog, run launching and run inspection (incl. artifacts).
package web

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // embed the IANA tz database; the static CGO_ENABLED=0 hub binary has no system zoneinfo for time.LoadLocation

	"github.com/vasyakrg/recon/internal/common/version"
	"github.com/vasyakrg/recon/internal/hub/investigator"
	"github.com/vasyakrg/recon/internal/hub/release"
	hubrunner "github.com/vasyakrg/recon/internal/hub/runner"
	"github.com/vasyakrg/recon/internal/hub/store"
)

//go:embed templates/*.html
var tplFS embed.FS

//go:embed static/*
var staticFS embed.FS

type Server struct {
	store        *store.Store
	runner       *hubrunner.Runner
	loop         *investigator.Loop // optional — nil when LLM is not configured
	availability InvestigatorAvailability
	llmProfiles  []LLMProfileView
	release      *release.Poller // optional — nil when release repo URL is unset/invalid
	tpl          *template.Template
	log          *slog.Logger
	auth         authConfig
	install      InstallConfig
	sessions     *sessionStore
	// nb reads investigation notebooks for the UI/API/export surfaces. It is
	// independent of loop (which is nil when the LLM is disabled) so past
	// investigations can still be exported/downloaded. Nil/unconfigured is a
	// safe no-op.
	nb *investigator.Notebook
}

// SetArtifactDir wires the artifact root so the web layer can read
// per-investigation notebooks. Call once at wiring time (cmd/hub/main.go).
func (s *Server) SetArtifactDir(dir string) {
	s.nb = investigator.NewNotebook(dir, s.log)
}

type LLMProfileView struct {
	Name                  string
	Role                  string
	Model                 string
	BaseURL               string
	ContextWindowTokens   int
	MaxOutputTokens       int
	SupportsTools         bool
	SupportsPromptCache   bool
	ContextWindowFallback bool
}

// InvestigatorAvailability is the operator-safe view of investigator state.
// DisabledReason is a reason class, not a raw provider error or URL.
type InvestigatorAvailability struct {
	Enabled        bool
	DisabledReason string
	ConfigHint     string
}

type terminalPayloadView struct {
	Present     bool
	Kind        string
	Reason      string
	Detail      string
	Recoverable bool
	Transient   bool
	Source      string
	// Summary is the structured mark_done conclusion (root cause, symptoms,
	// remediation, …) decoded from the terminal payload's `summary` field. It is
	// nil for legacy rows and for budget_finalize / operator_end / error
	// terminals that carry no structured diagnosis — the Conclusion card renders
	// only when this is non-nil, so all terminal kinds degrade gracefully.
	Summary *terminalSummaryView
}

// terminalSummaryView is the operator-facing projection of the mark_done
// summary schema (internal/hub/investigator/tools.go). Field order/names mirror
// the schema so the Conclusion card and the tool contract stay in lockstep.
type terminalSummaryView struct {
	RootCause              string
	RootCauseExplains      string
	Confidence             string
	Symptoms               []string
	HostsExamined          []string
	EvidenceRefs           []string
	WhereToLookNext        []string
	RecommendedRemediation string
}

func NewInvestigatorAvailability(loop *investigator.Loop, reason string) InvestigatorAvailability {
	if loop != nil {
		return InvestigatorAvailability{Enabled: true}
	}
	reason = sanitizeAvailabilityReason(reason)
	return InvestigatorAvailability{
		Enabled:        false,
		DisabledReason: reason,
		ConfigHint:     configHintForAvailabilityReason(reason),
	}
}

func (s *Server) SetLLMProfiles(profiles []LLMProfileView) {
	s.llmProfiles = append([]LLMProfileView(nil), profiles...)
}

// knownLLMProfile reports whether name matches a configured model profile.
// Used to validate the per-investigation routing override (Task 14).
func (s *Server) knownLLMProfile(name string) bool {
	for _, p := range s.llmProfiles {
		if p.Name == name {
			return true
		}
	}
	return false
}

func sanitizeAvailabilityReason(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch {
	case reason == "":
		return "not_configured"
	case strings.Contains(reason, "api key"):
		return "missing_api_key"
	case strings.Contains(reason, "plaintext http"):
		return "insecure_base_url"
	case strings.Contains(reason, "base_url") || strings.Contains(reason, "url"):
		return "invalid_base_url"
	case strings.Contains(reason, "model"):
		return "invalid_model"
	default:
		return "llm_client_init_failed"
	}
}

func configHintForAvailabilityReason(reason string) string {
	switch reason {
	case "missing_api_key":
		return "Set RECON_LLM_API_KEY and restart the hub. Confirm startup logs show LLM client ready."
	case "insecure_base_url":
		return "RECON_LLM_BASE_URL is plaintext HTTP. Use HTTPS, or set RECON_LLM_ALLOW_INSECURE_HTTP=true only when the endpoint is a private/link-local router IP, then restart the hub. Public HTTP URLs and HTTP hostnames remain rejected."
	case "invalid_base_url":
		return "Set RECON_LLM_BASE_URL to a valid HTTPS OpenAI-compatible endpoint, or to an allowed private/link-local HTTP router IP with RECON_LLM_ALLOW_INSECURE_HTTP=true, then restart the hub."
	case "invalid_model":
		return "Set RECON_LLM_MODEL to a model served by the configured LLM endpoint, then restart the hub. Check router/provider logs for model-name errors."
	case "not_configured":
		return "Set RECON_LLM_API_KEY, RECON_LLM_BASE_URL, and RECON_LLM_MODEL, then restart the hub. Confirm startup logs show LLM client ready."
	default:
		return "Check the sanitized startup log fields reason_class, llm_model, llm_base_url_scheme, and llm_base_url_host; fix the LLM config and restart the hub."
	}
}

// InstallConfig surfaces the hub's "Quick install" knobs to the web layer:
// the GitHub repo whose releases ship the agent tarball, the host:port the
// agent should configure as its hub endpoint, and which release version to
// install ("latest" or a tag like "0.1.0"). RepoURL + Endpoint must both be
// set for the /hosts "Quick install" form to render.
type InstallConfig struct {
	ReleaseRepoURL string
	// AgentGRPCEndpoint is host:port the agent dials. Set to "auto" (or
	// leave empty) to derive from the install URL's request hostname plus
	// GRPCPort — works when the same machine hosts both the UI and the
	// gRPC port, which is the common compose / single-VM case.
	AgentGRPCEndpoint string
	GRPCPort          int
	Version           string
	// ExternalURL is the public URL the install one-liner uses
	// (scheme://host[:port]). Wins over the auto-derive from request
	// host. Set when the operator UI path and the agent network path
	// don't share the same hostname/port (e.g. orbstack autoroute on
	// :443 vs nginx exposed on :8443).
	ExternalURL string
	// TrustedTLS=true tells the install one-liner to verify the hub's
	// TLS cert (drops curl's `-k` flag). Default is false because the
	// `make compose-up` path generates a self-signed cert and any
	// unconfigured deployment would otherwise refuse to install. Flip
	// to true once you front the hub with a real CA-issued cert —
	// keeps the script fetch from being silently substitutable by a
	// MITM (review M2).
	TrustedTLS bool
	// SelfHosted=true makes the hub serve the agent tarballs/checksums and a
	// releases/latest JSON from its own /releases route (SH2/SH3) instead of
	// GitHub. The install one-liner and the agent's self-updater are then
	// pointed at the hub's public base. GitHub mode stays available when
	// false (SH5).
	SelfHosted bool
	// ReleasesDir is the on-disk directory the /releases route serves from
	// (SH2). Baked into the image at /usr/local/share/recon/releases;
	// overridable for tests. Empty → /releases asset requests 404.
	ReleasesDir string
}

// Enabled returns true when the install one-liner can be served. In GitHub
// mode that needs a release repo URL; in self-hosted mode the hub serves the
// agent from its own /releases route, so it's always enabled regardless of
// release_repo_url (which may legitimately be empty — SH5).
func (i InstallConfig) Enabled() bool {
	return i.SelfHosted || i.ReleaseRepoURL != ""
}

// DownloadBase returns the GitHub directory the install script pulls the
// tarball from, branching on Version. Self-hosted installs do NOT use this —
// they resolve the base from the hub's public origin at request time (see
// downloadBaseURL + handleInstallAgentScript), because the hub URL isn't known
// until the request arrives when external_url is left empty.
func (i InstallConfig) DownloadBase() string {
	return downloadBaseURL(i.ReleaseRepoURL, i.Version)
}

// downloadBaseURL composes the release "download base" for a given root origin
// (a GitHub repo URL or the hub's public base) and version. "latest" (or
// empty) → <root>/releases/latest/download; otherwise →
// <root>/releases/download/v<version>. The leading "v" is added when missing
// so operators can write either "0.1.0" or "v0.1.0".
func downloadBaseURL(root, version string) string {
	root = strings.TrimRight(root, "/")
	if version == "" || version == "latest" {
		return root + "/releases/latest/download"
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return root + "/releases/download/" + version
}

// AuthConfig is the public knob set by cmd/hub. Username + bcrypt password
// hash come from env / yaml; SessionTTL defaults to 12h.
type AuthConfig struct {
	Username       string
	PasswordHash   string
	SessionTTL     time.Duration
	BehindTLSProxy bool
}

func (a AuthConfig) Enabled() bool { return a.Username != "" && a.PasswordHash != "" }

// GenPasswordHash exposes the bcrypt helper to cmd/hub.
func GenPasswordHash(pw string) (string, error) { return PasswordHashFromPlaintext(pw) }

func NewServer(st *store.Store, runner *hubrunner.Runner, loop *investigator.Loop, availability InvestigatorAvailability,
	rel *release.Poller, auth AuthConfig, install InstallConfig, log *slog.Logger) (*Server, error) {
	assetVer := assetVersion()
	tpl, err := template.New("").Funcs(template.FuncMap{
		"assetVer":   func() string { return assetVer },
		"prettyJSON": prettyJSON,
		"truncate":   truncate,
		// md / mdLine render investigator free-text as Markdown. SAFE by
		// construction (escape-first, fixed tag allowlist) — see markdown.go.
		// md = block (paragraphs, lists, code fences); mdLine = inline only.
		"md":      renderMarkdownBlock,
		"mdLine":  renderMarkdownInline,
		"bytesOf": func(s string) []byte { return []byte(s) },
		"mapJSON": func(m map[string]any) string {
			b, _ := json.MarshalIndent(m, "", "  ")
			return string(b)
		},
		"compactNum":      compactNum,
		"shortID":         shortID,
		"sinceUTC":        sinceUTC,
		"formatUserTZ":    formatUserTZ,
		"confidenceBadge": confidenceBadge,
		// splitComma splits a comma-joined task_id (collect_batch rows store
		// strings.Join(ids, ",")) so the timeline can emit one evidence anchor per
		// individual id — a mark_done evidence_ref is always a single task_id.
		"splitComma": func(s string) []string {
			if s == "" {
				return nil
			}
			return strings.Split(s, ",")
		},
		"askQuestion": func(inputJSON string) string {
			var a struct {
				Question string `json:"question"`
			}
			_ = json.Unmarshal([]byte(inputJSON), &a)
			return a.Question
		},
		// markDoneConclusion decodes a pending mark_done's proposed conclusion
		// ({"summary":{…}}) so the held terminal-close card can surface the root
		// cause + confidence instead of only raw JSON. Returns nil on a malformed
		// or empty proposal (card falls back to the editable JSON only).
		"markDoneConclusion": func(inputJSON string) *terminalSummaryView {
			var a struct {
				Summary json.RawMessage `json:"summary"`
			}
			if err := json.Unmarshal([]byte(inputJSON), &a); err != nil {
				return nil
			}
			return parseTerminalSummary(a.Summary)
		},
		"add": func(a, b int) int { return a + b },
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mul":        func(a, b int) int { return a * b },
		"pct":        pct,
		"findCount":  findCount,
		"barRepeat":  barRepeat,
		"replaceAll": strings.ReplaceAll,
		"now":        func() time.Time { return time.Now().UTC() },
		"outdated":   release.Outdated,
	}).ParseFS(tplFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	if auth.SessionTTL <= 0 {
		auth.SessionTTL = 12 * time.Hour
	}
	if availability.Enabled != (loop != nil) || (!availability.Enabled && availability.DisabledReason == "") {
		availability = NewInvestigatorAvailability(loop, availability.DisabledReason)
	}
	return &Server{
		store: st, runner: runner, loop: loop, availability: availability, release: rel, tpl: tpl, log: log,
		auth:     authConfig(auth),
		install:  install,
		sessions: newSessionStore(st),
	}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	// Public endpoints (no auth check, no CSRF).
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/static/", staticHandler())
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	// Public install endpoint — no session, no CSRF. Authentication is the
	// single-use bootstrap token embedded in the URL itself, validated by
	// the agent's Enroll RPC at install time.
	mux.HandleFunc("/install/agent.sh", s.handleInstallAgentScript)
	// Public self-hosted agent distribution — tarballs, checksums, and a
	// GitHub-API-shaped releases/latest JSON. No auth: the bundled agent is
	// not a secret and the install / self-update flow needs it before any
	// token exchange (SH2/SH3).
	mux.HandleFunc("/releases/", s.handleReleases)

	// Authenticated endpoints. requireAuth is a no-op when auth is not
	// configured (single-trust loopback mode).
	auth := s.requireAuth
	mux.HandleFunc("/", auth(s.handleRoot))
	mux.HandleFunc("/hosts", auth(s.handleHosts))
	mux.HandleFunc("/hosts/", auth(s.handleHostDetail))
	mux.HandleFunc("/collectors", auth(s.handleCollectorsCatalog))
	mux.HandleFunc("/runs", auth(s.handleRunsList))
	mux.HandleFunc("/runs/", auth(s.handleRunsDetail))
	mux.HandleFunc("/runs/new", auth(s.handleRunsNew))
	mux.HandleFunc("/investigations", auth(s.handleInvestigationsList))
	mux.HandleFunc("/investigations/", auth(s.handleInvestigationsDetail))
	mux.HandleFunc("/investigations/new", auth(s.handleInvestigationsNew))
	mux.HandleFunc("/investigations/decide", auth(s.handleInvestigationDecide))
	mux.HandleFunc("/investigations/answer", auth(s.handleInvestigationAnswer))
	mux.HandleFunc("/investigations/hypothesis", auth(s.handleHypothesis))
	mux.HandleFunc("/investigations/continue", auth(s.handleInvestigationContinue))
	mux.HandleFunc("/investigations/retry", auth(s.handleInvestigationRetry))
	mux.HandleFunc("/investigations/extend", auth(s.handleInvestigationExtend))
	mux.HandleFunc("/investigations/finalize", auth(s.handleInvestigationFinalize))
	mux.HandleFunc("/investigations/auto-approve", auth(s.handleInvestigationAutoApprove))
	mux.HandleFunc("/investigations/autonomous", auth(s.handleInvestigationAutonomous))
	mux.HandleFunc("/findings/", auth(s.handleFindingAction))
	mux.HandleFunc("/investigations/export/", auth(s.handleInvestigationExport))
	mux.HandleFunc("/investigations/notebook/", auth(s.handleInvestigationNotebook))
	mux.HandleFunc("/investigations/events/", auth(s.handleInvestigationSSE))
	mux.HandleFunc("/investigations/fragments/", auth(s.handleInvestigationFragments))
	mux.HandleFunc("/audit", auth(s.handleAudit))
	mux.HandleFunc("/settings", auth(s.handleSettings))
	mux.HandleFunc("/settings/issue-token", auth(s.handleIssueToken))
	mux.HandleFunc("/settings/revoke-token", auth(s.handleRevokeToken))
	mux.HandleFunc("/settings/api-tokens/issue", auth(s.handleIssueAPIToken))
	mux.HandleFunc("/settings/api-tokens/revoke", auth(s.handleRevokeAPIToken))
	mux.HandleFunc("/hosts/delete", auth(s.handleHostDelete))
	mux.HandleFunc("/hosts/revoke", auth(s.handleHostRevoke))
	mux.HandleFunc("/hosts/quick-install", auth(s.handleQuickInstall))

	// /api/v1/* — Bearer-auth JSON, no cookie/CSRF chain.
	s.registerAPIRoutes(mux)
	return mux
}

func (s *Server) Serve(ctx context.Context, addr string) error {
	return s.ServeTLS(ctx, addr, "", "")
}

// ServeTLS starts the HTTP listener. When certFile and keyFile are both
// non-empty the listener terminates TLS itself; otherwise plain HTTP is
// served and operators are expected to front it with nginx (preserving
// the legacy compose topology).
func (s *Server) ServeTLS(ctx context.Context, addr, certFile, keyFile string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	//nolint:gosec // G118: parent ctx is already done before Shutdown; need fresh ctx for graceful drain.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go s.runSessionGC(ctx)

	tls := certFile != "" && keyFile != ""
	s.log.Info("web listening", "addr", addr, "auth", s.auth.Enabled(), "tls", tls)
	var err error
	if tls {
		err = srv.ListenAndServeTLS(certFile, keyFile)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/hosts", http.StatusFound)
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Pull the freshly-issued install one-liner out of the session flash, if
	// the operator just submitted the Quick install form.
	var oneLiner, installAgentID string
	if sid, err := r.Cookie(cookieSession); err == nil && sid != nil {
		oneLiner = s.sessions.popFlash(sid.Value, "install_one_liner")
		installAgentID = s.sessions.popFlash(sid.Value, "install_agent_id")
	}
	latest, releasesURL := "", ""
	if s.release != nil {
		latest, _ = s.release.Latest()
		releasesURL = s.release.ReleasesURL()
	}
	s.renderForReq(w, r, "hosts", map[string]any{
		"Title":              "Hosts",
		"Hosts":              hosts,
		"InstallEnabled":     s.install.Enabled(),
		"InstallOneLiner":    oneLiner,
		"InstallAgentID":     installAgentID,
		"LatestVersion":      latest,
		"ReleasesURL":        releasesURL,
		"UpdateDownloadBase": s.agentDownloadBase(r, releasesURL),
	})
}

func (s *Server) handleHostDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/hosts/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	host, err := s.store.GetHost(r.Context(), id)
	if err != nil {
		http.Error(w, "host not found", http.StatusNotFound)
		return
	}
	mans, _ := s.store.ListCollectorManifests(r.Context(), id)
	latest, releasesURL := "", ""
	if s.release != nil {
		latest, _ = s.release.Latest()
		releasesURL = s.release.ReleasesURL()
	}
	s.renderForReq(w, r, "host_detail", map[string]any{
		"Title":              "Host " + id,
		"Version":            version.Version,
		"ContentBlock":       "host_detail",
		"Host":               host,
		"Collectors":         mans,
		"LatestVersion":      latest,
		"ReleasesURL":        releasesURL,
		"UpdateDownloadBase": s.agentDownloadBase(r, releasesURL),
	})
}

func (s *Server) handleCollectorsCatalog(w http.ResponseWriter, r *http.Request) {
	hosts, _ := s.store.ListHosts(r.Context())
	type entry struct {
		Name        string
		Hosts       []string
		HostCount   int
		TotalAgents int
		Latest      string
		Category    string
		Description string
		Reads       []string
		Requires    []string
		SchemaJSON  string
	}
	byName := map[string]*entry{}
	for _, h := range hosts {
		mans, _ := s.store.ListCollectorManifests(r.Context(), h.ID)
		for _, m := range mans {
			e, ok := byName[m.Name]
			if !ok {
				e = &entry{Name: m.Name}
				byName[m.Name] = e
			}
			e.Hosts = append(e.Hosts, h.ID)
			e.Latest = m.Version
			// Pull richer metadata out of the embedded manifest JSON; keep
			// best-effort — older agents may emit minimal manifests.
			var raw struct {
				Category    string         `json:"category"`
				Description string         `json:"description"`
				Reads       []string       `json:"reads"`
				Requires    []string       `json:"requires"`
				Schema      map[string]any `json:"input_schema"`
			}
			if err := json.Unmarshal(m.ManifestJSON, &raw); err == nil {
				if e.Category == "" {
					e.Category = raw.Category
				}
				if e.Description == "" {
					e.Description = raw.Description
				}
				if len(e.Reads) == 0 {
					e.Reads = raw.Reads
				}
				if len(e.Requires) == 0 {
					e.Requires = raw.Requires
				}
				if e.SchemaJSON == "" && raw.Schema != nil {
					if b, err := json.MarshalIndent(raw.Schema, "", "  "); err == nil {
						e.SchemaJSON = string(b)
					}
				}
			}
		}
	}
	var entries []*entry
	for _, e := range byName {
		e.HostCount = len(e.Hosts)
		e.TotalAgents = len(hosts)
		entries = append(entries, e)
	}
	// Stable order: alpha by name. Inline insertion sort — list is ≤30 rows.
	for i := 1; i < len(entries); i++ {
		j := i
		for j > 0 && entries[j-1].Name > entries[j].Name {
			entries[j-1], entries[j] = entries[j], entries[j-1]
			j--
		}
	}
	s.renderForReq(w, r, "collectors", map[string]any{
		"Title":   "Collectors",
		"Entries": entries,
	})
}

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderForReq(w, r, "runs_list", map[string]any{
		"Title":        "Runs",
		"Version":      version.Version,
		"ContentBlock": "runs_list",
		"Runs":         runs,
	})
}

func (s *Server) handleRunsDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/runs/")
	if rest == "" || rest == "new" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	runID := parts[0]
	if len(parts) == 2 && strings.HasPrefix(parts[1], "artifact/") {
		s.serveArtifact(w, r, runID, strings.TrimPrefix(parts[1], "artifact/"))
		return
	}
	run, err := s.store.GetRun(r.Context(), runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tasks, _ := s.store.ListTasks(r.Context(), runID)
	type tview struct {
		store.Task
		Result *store.Result
	}
	views := make([]tview, 0, len(tasks))
	collector := ""
	var ok, errCnt, pending int
	for _, t := range tasks {
		v := tview{Task: t}
		if res, err := s.store.GetResult(r.Context(), t.ID); err == nil {
			v.Result = &res
		}
		views = append(views, v)
		if collector == "" {
			collector = t.Collector
		}
		switch t.Status {
		case "ok":
			ok++
		case "pending", "sent":
			pending++
		default:
			errCnt++
		}
	}
	s.renderForReq(w, r, "run_detail", map[string]any{
		"Title":        "Run " + runID,
		"Run":          run,
		"Tasks":        views,
		"Collector":    collector,
		"OkCount":      ok,
		"ErrCount":     errCnt,
		"PendingCount": pending,
	})
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, taskID, name string) {
	res, err := s.store.GetResult(r.Context(), taskID)
	if err != nil || res.ArtifactDir == "" {
		http.NotFound(w, r)
		return
	}
	clean := filepath.Clean(filepath.Join(res.ArtifactDir, name))
	if !strings.HasPrefix(clean, filepath.Clean(res.ArtifactDir)+string(os.PathSeparator)) {
		http.Error(w, "path traversal", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, clean)
}

func (s *Server) handleRunsNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024) // hard cap on form size
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hosts := r.Form["host_id"]
	collector := r.FormValue("collector")
	if len(hosts) == 0 || collector == "" {
		http.Error(w, "host_id and collector required", http.StatusBadRequest)
		return
	}
	params := map[string]string{}
	for k, v := range r.Form {
		if strings.HasPrefix(k, "param_") && len(v) > 0 && v[0] != "" {
			params[strings.TrimPrefix(k, "param_")] = v[0]
		}
	}
	runID, err := s.runner.CreateRun(r.Context(), hubrunner.RunRequest{
		Name:      r.FormValue("name"),
		HostIDs:   hosts,
		Collector: collector,
		Params:    params,
		CreatedBy: "operator", // week 2: no auth, see plan
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(r.Context(), "operator", "run.create",
		map[string]any{"run_id": runID, "collector": collector, "host_count": len(hosts)})
	http.Redirect(w, r, "/runs/"+runID, http.StatusSeeOther)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	hosts, _ := s.store.ListHosts(r.Context())
	// (review C1) Read freshly issued token from server-side flash, NOT
	// from URL query — putting secrets in URLs leaks them to nginx
	// access_log, browser history, Referer headers.
	issued := ""
	if sid, err := r.Cookie(cookieSession); err == nil && sid != nil {
		issued = s.sessions.popFlash(sid.Value, "issued_token")
	}
	model, baseURL := "", ""
	maxSteps, maxTokens := 0, 0
	if s.loop != nil {
		model, baseURL = s.loop.Info()
		maxSteps, maxTokens = s.loop.Budgets()
	}
	tokens, _ := s.store.ListBootstrapTokens(r.Context(), 50)
	apiTokens, _ := s.store.ListAPITokens(r.Context(), 100)
	issuedAPI := ""
	if sid, err := r.Cookie(cookieSession); err == nil && sid != nil {
		issuedAPI = s.sessions.popFlash(sid.Value, "issued_api_token")
	}
	s.renderForReq(w, r, "settings", map[string]any{
		"Title":          "Settings",
		"Hosts":          hosts,
		"Issued":         issued,
		"IssuedAPIToken": issuedAPI,
		"Model":          model,
		"BaseURL":        baseURL,
		"Investigator":   s.availability,
		"LLMProfiles":    s.llmProfiles,
		"MaxSteps":       maxSteps,
		"MaxTokens":      maxTokens,
		"AdminUser":      s.auth.Username,
		"Tokens":         tokens,
		"APITokens":      apiTokens,
	})
}

func (s *Server) handleIssueAPIToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	scope := strings.TrimSpace(r.FormValue("scope"))
	ttlS := strings.TrimSpace(r.FormValue("ttl"))
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if !store.ValidAPIScope(scope) {
		http.Error(w, "invalid scope", http.StatusBadRequest)
		return
	}
	var expires sql.NullTime
	if ttlS != "" {
		d, err := time.ParseDuration(ttlS)
		if err != nil || d <= 0 || d > 10*365*24*time.Hour {
			http.Error(w, "invalid ttl", http.StatusBadRequest)
			return
		}
		expires = sql.NullTime{Time: time.Now().UTC().Add(d), Valid: true}
	}
	raw, hash, prefix, err := store.GenerateAPIToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := s.store.InsertAPIToken(r.Context(), name, hash, prefix, scope, authedUser(r), expires)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), authedUser(r), "api_token.issue",
		map[string]any{"id": id, "name": name, "scope": scope})
	if sid, err := r.Cookie(cookieSession); err == nil && sid != nil {
		s.sessions.setFlash(sid.Value, "issued_api_token", raw)
	}
	http.Redirect(w, r, "/settings#api-tokens", http.StatusSeeOther)
}

func (s *Server) handleRevokeAPIToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if err := s.store.RevokeAPIToken(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), authedUser(r), "api_token.revoke", map[string]any{"id": id})
	http.Redirect(w, r, "/settings#api-tokens", http.StatusSeeOther)
}

func (s *Server) handleIssueToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentID := strings.TrimSpace(r.FormValue("agent_id"))
	ttlS := r.FormValue("ttl")
	if agentID == "" {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	ttl := 24 * time.Hour
	if ttlS != "" {
		if d, err := time.ParseDuration(ttlS); err == nil && d > 0 && d <= 30*24*time.Hour {
			ttl = d
		}
	}
	tok, err := investigatorTokenFor(r.Context(), s, agentID, ttl, authedUser(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), authedUser(r), "token.issue",
		map[string]any{"agent_id": agentID, "ttl": ttl.String()})
	// (review C1) Stash the freshly-issued token in server-side flash so
	// the redirect URL stays clean (nginx logs / browser history / Referer).
	if sid, err := r.Cookie(cookieSession); err == nil && sid != nil {
		s.sessions.setFlash(sid.Value, "issued_token", tok)
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleRevokeToken deletes an unused (or any) bootstrap token by hash.
// The hash is the primary key — operators never see the plaintext after
// issue, so the form passes the hash directly.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hash := r.FormValue("token_hash")
	if hash == "" {
		http.Error(w, "token_hash required", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteBootstrapToken(r.Context(), hash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), authedUser(r), "token.revoke", map[string]any{"token_hash_prefix": truncate(hash, 12)})
	http.Redirect(w, r, "/settings#tokens", http.StatusSeeOther)
}

// handleHostRevoke marks the agent's enrolled identity revoked. The next
// gRPC Connect from that agent fails with `agent identity revoked`. The
// host row stays — operator can re-enroll under the same id with a fresh
// bootstrap token.
func (s *Server) handleHostRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("agent_id")
	if id == "" {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	reason := r.FormValue("reason")
	if reason == "" {
		reason = "operator UI"
	}
	if err := s.store.RevokeIdentity(r.Context(), id, reason); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), authedUser(r), "identity.revoke", map[string]any{"agent_id": id, "reason": reason})
	http.Redirect(w, r, "/hosts/"+id, http.StatusSeeOther)
}

// handleHostDelete wipes the host + its enrollment row + cascades through
// collector_manifests / tasks. Refuses to delete an online host so the
// operator can't accidentally orphan a running agent — revoke first.
func (s *Server) handleHostDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("agent_id")
	if id == "" {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	host, err := s.store.GetHost(r.Context(), id)
	if err != nil {
		http.Error(w, "host not found", http.StatusNotFound)
		return
	}
	if host.Status == "online" {
		http.Error(w, "refusing to delete an online host — revoke its identity first, wait for it to drop offline", http.StatusConflict)
		return
	}
	if err := s.store.DeleteHost(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), authedUser(r), "host.delete", map[string]any{"agent_id": id, "last_status": host.Status})
	http.Redirect(w, r, "/hosts", http.StatusSeeOther)
}

// audit writes an audit row, escalating any failure to ERROR-level slog —
// audit is the one table where silent loss is unacceptable (review H2).
func (s *Server) audit(ctx context.Context, actor, action string, details map[string]any) {
	if err := s.store.AuditLog(ctx, actor, action, details); err != nil {
		s.log.Error("audit write failed", "actor", actor, "action", action, "err", err)
	}
}

// renderForReq variant that injects the per-session CSRF token into the
// data map so templates can embed `<input name="csrf">`. Used by all
// authenticated GET handlers that render forms.
func (s *Server) renderForReq(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["CSRF"] = s.csrfTokenFor(r)
	data["AuthEnabled"] = s.auth.Enabled()
	data["Username"] = authedUser(r)
	data["UserTZ"] = userLocation(r)
	if _, ok := data["ActiveNav"]; !ok {
		data["ActiveNav"] = activeNavFor(page)
	}
	if _, ok := data["Title"]; !ok {
		data["Title"] = page
	}
	if _, ok := data["Version"]; !ok {
		data["Version"] = version.Version
	}
	if cnt, err := s.store.NavCounts(r.Context()); err == nil {
		data["HostCount"] = cnt.Hosts
		data["InvestigationsActive"] = cnt.InvestigationsActive
		data["CollectorCount"] = cnt.Collectors
	}
	s.render(w, page, data)
}

// activeNavFor maps an internal page name to a sidebar nav key so the layout
// can highlight the right item without each handler having to set it.
func activeNavFor(page string) string {
	switch page {
	case "hosts", "host_detail":
		return "hosts"
	case "collectors":
		return "collectors"
	case "runs_list", "run_detail":
		return "runs"
	case "investigations_list", "investigation_detail":
		return "investigations"
	case "audit":
		return "audit"
	case "settings":
		return "settings"
	}
	return ""
}

// staticHandler serves embedded /static/* assets with conservative caching.
// Strips the URL prefix so the FS path mirrors the embed layout. Safe to
// cache for 5 minutes because asset URLs are content-versioned via the
// assetVer query param (see layout.html) — a new build serves a new URL, so
// the browser never holds a stale hub.js/hub.css after a deploy.
func staticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	fs := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		fs.ServeHTTP(w, r)
	}))
}

// assetVersion is a short content hash of the front-end assets, used to
// cache-bust /static/hub.js and /static/hub.css on every build. Without it a
// deploy that changes hub.js leaves the browser serving the cached old file
// (max-age above) until it expires — the exact reason a fixed UI can still
// look broken after a deploy.
func assetVersion() string {
	h := sha256.New()
	for _, name := range []string{"static/hub.js", "static/hub.css"} {
		if b, err := staticFS.ReadFile(name); err == nil {
			h.Write(b)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// renderStandalone executes a complete page template by name without
// wrapping it in the global layout/sidebar shell. Used for chrome-less
// pages like /login. We Clone() rather than execute the shared root
// directly: html/template forbids Clone after ExecuteTemplate, so reusing
// the root would break every subsequent layout-based render.
func (s *Server) renderStandalone(w http.ResponseWriter, page string, data any) {
	t, err := s.tpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, page, data); err != nil {
		s.log.Error("render standalone", "page", page, "err", err)
	}
}

// render executes layout.html, dynamically aliasing the "content" block to
// the per-page template. Each page template defines a uniquely-named block
// (e.g. "hosts", "run_detail") so they don't clash; the alias is set per
// request on a clone of the parsed set.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, err := s.tpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Wrap each per-page template in <div class="pg-body"> so individual
	// pages don't need to know about the layout chrome. Per-page header
	// strips (.pg-hd) live inside the page template when needed.
	if _, err := t.New("content").Parse(fmt.Sprintf(`<div class="pg-body">{{template %q .}}</div>`, page)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

// renderFragment executes a named live-update partial on a CLONE of the
// parsed set. It must clone rather than execute s.tpl directly:
// html/template forbids Clone() after a template has executed, so executing
// the shared root here would poison every subsequent layout/standalone render
// (symptom: "html/template: cannot Clone \"\" after it has executed" on the
// next full page load — e.g. the redirect right after Approve).
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	t, err := s.tpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render fragment", "template", name, "err", err)
	}
}

// regionHash renders a named live sub-fragment and returns a short content
// fingerprint of its rendered HTML. The client live engine compares this per
// region (data-frag-hash) and skips swapping a region whose hash is unchanged,
// so the volatile token/budget bars — which live ONLY in the status fragment —
// no longer reflow the timeline and side regions on every poll tick. That
// over-broad reflow (driven by the single global data-snapshot fingerprint that
// embeds updated_at + token counters) is the reported flicker. Hashing the
// rendered HTML captures exactly what the operator sees; the timeline/side
// fragments contain no token counters and no relative timestamps (sinceUTC is
// only in the static goal header, outside every live region), so their hashes
// stay stable across token churn — which is what kills the flicker.
func (s *Server) regionHash(name string, data any) string {
	t, err := s.tpl.Clone()
	if err != nil {
		return ""
	}
	var sb strings.Builder
	if err := t.ExecuteTemplate(&sb, name, data); err != nil {
		if s.log != nil {
			s.log.Debug("region hash render failed", "template", name, "err", err)
		}
		return ""
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return fmt.Sprintf("%x", sum[:8])
}

// sideRegionHash fingerprints the side panel's discrete, operator-meaningful
// content (status, findings + their pin/ignore state, tool-call and memory
// counts) while deliberately EXCLUDING the volatile "tokens used" / compaction
// counters the side Context panel also displays. Those churn every LLM turn and
// are shown live in the status budget bar; hashing the rendered side fragment
// would reflow the findings list and collapse expanded evidence on every poll
// tick (the flicker). A structured hash is used here precisely so those
// volatile duplicates can be left out while real findings/memory/tool-call
// changes still move the hash.
func sideRegionHash(status string, toolCallCount, memoryCount int, findings []store.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "st=%s|tc=%d|mem=%d|find=%d|", status, toolCallCount, memoryCount, len(findings))
	for _, f := range findings {
		fmt.Fprintf(&b, "%s:%t:%t:%s;", f.ID, f.Pinned, f.Ignored, f.Severity)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum[:8])
}

// priorChoice is the display shape for a prior investigation, used by both the
// create-form picker (manual attach) and the detail "Prior investigations"
// panel (visibility).
type priorChoice struct {
	ID         string
	Goal       string
	Status     string
	Date       string
	Hosts      string
	RootCause  string
	Confidence string
}

// priorChoicesFrom turns store prior records into display rows. It surfaces the
// FULL structured root_cause (the operator wants to read it, not a 240-char
// stub), falling back to the truncated terminal reason for legacy/aborted
// payloads that carry no structured summary.
func priorChoicesFrom(ps []store.PriorInvestigation) []priorChoice {
	out := make([]priorChoice, 0, len(ps))
	for _, p := range ps {
		rc := ""
		conf := ""
		if pl, ok := store.ParseInvestigationTerminalPayload(p.SummaryJSON); ok {
			if sv := parseTerminalSummary(pl.Summary); sv != nil && strings.TrimSpace(sv.RootCause) != "" {
				rc = strings.TrimSpace(sv.RootCause)
				conf = sv.Confidence
			} else {
				// Legacy / no structured summary: the one-line (already-capped) reason.
				rc = strings.TrimSpace(strings.TrimPrefix(strings.Join(strings.Fields(pl.Reason), " "), "Investigation complete:"))
			}
		}
		hosts := "all hosts"
		if len(p.AllowedHosts) > 0 {
			hosts = strings.Join(p.AllowedHosts, ", ")
		}
		date := ""
		if !p.CreatedAt.IsZero() {
			date = p.CreatedAt.UTC().Format("2006-01-02")
		}
		out = append(out, priorChoice{ID: p.ID, Goal: p.Goal, Status: p.Status, Date: date, Hosts: hosts, RootCause: rc, Confidence: conf})
	}
	return out
}

// compactNum renders large integers as "48.2k" / "1.2m" — used in the
// investigation list token columns and budget bars.
func compactNum(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// shortID truncates a long opaque ID for display, keeping the tail visually
// distinguishable. "inv_a00000000007" → "inv_a0000000…".
func shortID(s string, keep int) string {
	if len(s) <= keep {
		return s
	}
	return s[:keep] + "…"
}

// cookieTZ holds the operator's IANA timezone, set client-side from the
// browser's Intl resolved zone. Auth is optional, so the preference rides a
// plain cookie rather than a server session row.
const cookieTZ = "tz"

// userLocation resolves the operator's display timezone from the tz cookie.
// Falls back to UTC for a missing / malformed / unknown zone. Requires the
// embedded IANA database (see the time/tzdata blank import) because the static
// hub binary has no system zoneinfo. The result drives formatUserTZ; all
// LLM-facing, notebook, export, and JSON-API timestamps stay UTC (invariant 5).
func userLocation(r *http.Request) *time.Location {
	c, err := r.Cookie(cookieTZ)
	if err != nil || c == nil || c.Value == "" {
		return time.UTC
	}
	// The browser sets the cookie via encodeURIComponent, so a multi-component
	// IANA zone arrives percent-encoded ("America%2FNew_York"); Go's r.Cookie
	// does NOT decode it. PathUnescape (not QueryUnescape — that would turn the
	// '+' in Etc/GMT+5 into a space) restores the real name. A raw, un-encoded
	// value (no '%') passes through unchanged.
	name := c.Value
	if dec, derr := url.PathUnescape(name); derr == nil {
		name = dec
	}
	if !validTZName(name) {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// validTZName guards LoadLocation against a hostile cookie: IANA zone names are
// short ASCII over [A-Za-z0-9_+-/]. Anything else falls back to UTC.
func validTZName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '_' || r == '+' || r == '-':
		default:
			return false
		}
	}
	return true
}

// formatUserTZ renders a timestamp in the operator's timezone using a Go layout.
// A nil location (UserTZ unset) or zero time degrade to UTC / an em dash. Use a
// layout with the MST zone token (e.g. "2006-01-02 15:04:05 MST") where the zone
// should be shown — it renders the operator's zone abbreviation, "UTC" when the
// location is UTC. Only the live web views localize; everything machine-facing
// stays UTC.
func formatUserTZ(loc *time.Location, t time.Time, layout string) string {
	if t.IsZero() {
		return "—"
	}
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format(layout)
}

// sinceUTC formats a timestamp as a humanish "12s ago" / "1h ago" string for
// list views. Past 24h falls back to the absolute date.
func sinceUTC(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.UTC().Format("2006-01-02")
}

func pct(used, max int) int {
	if max <= 0 {
		return 0
	}
	p := used * 100 / max
	if p > 100 {
		return 100
	}
	return p
}

// findCount safely fetches a severity bucket from the per-investigation
// counts map; templates can't index a map[string]FindingCounts directly
// because the value type is a struct so we return ints here.
func findCount(counts map[string]store.FindingCounts, invID, severity string) int {
	c := counts[invID]
	switch severity {
	case "critical":
		return c.Critical
	case "error":
		return c.Error
	case "warn":
		return c.Warn
	case "info":
		return c.Info
	}
	return 0
}

// barRepeat returns a slice of length n so a template can {{range}} to draw
// a stripe per finding without arithmetic in HTML.
func barRepeat(n int) []struct{} {
	if n > 24 {
		n = 24
	}
	return make([]struct{}, n)
}

// prettyJSON formats raw JSON bytes for display. Best-effort — returns the
// input as a string on parse error.
func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- Investigations -----------------------------------------------------

func (s *Server) handleInvestigationsList(w http.ResponseWriter, r *http.Request) {
	invs, err := s.store.ListInvestigations(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	counts, _ := s.store.FindingCountsByInvestigation(r.Context())
	maxSteps, maxTokens := 0, 0
	if s.loop != nil {
		maxSteps, maxTokens = s.loop.Budgets()
	}
	// Filter chip selection — keep state in URL so links/back-button work.
	filter := r.URL.Query().Get("f")
	var statusBuckets = struct{ All, Active, Done, Aborted int }{}
	for _, i := range invs {
		statusBuckets.All++
		switch i.Status {
		case "active", "waiting":
			statusBuckets.Active++
		case "done":
			statusBuckets.Done++
		case "aborted":
			statusBuckets.Aborted++
		}
	}
	hosts, _ := s.store.ListHosts(r.Context())
	// Candidates for the optional "attach prior investigations" picker on the
	// create form (only when priors injection is enabled). ANY status — done
	// runs attach a conclusion, aborted/active runs attach their findings.
	var priorCandidates []priorChoice
	priorsEnabled := s.loop != nil && s.loop.PriorsEnabled()
	if priorsEnabled {
		dp, _ := s.store.ListRecentInvestigationsForPriors(r.Context(), "", 20)
		priorCandidates = priorChoicesFrom(dp)
	}
	s.renderForReq(w, r, "investigations_list", map[string]any{
		"Title":           "Investigations",
		"Items":           invs,
		"FindingCounts":   counts,
		"Investigator":    s.availability,
		"LLMEnabled":      s.availability.Enabled,
		"MaxSteps":        maxSteps,
		"MaxTokens":       maxTokens,
		"Filter":          filter,
		"Buckets":         statusBuckets,
		"Hosts":           hosts,
		"LLMProfiles":     s.llmProfiles,
		"PriorCandidates": priorCandidates,
		"PriorsEnabled":   priorsEnabled,
	})
}

func (s *Server) handleInvestigationsNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.availability.Enabled {
		s.log.Info("new investigation rejected", "failure_class", "disabled", "reason_class", s.availability.DisabledReason)
		http.Error(w, "investigator disabled — "+s.availability.ConfigHint, http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	goal := strings.TrimSpace(r.FormValue("goal"))
	if goal == "" {
		http.Error(w, "goal required", http.StatusBadRequest)
		return
	}
	// agent_ids is a multi-value form field (one <input name="agent_ids"
	// value="..."> per ticked checkbox). Empty = no scope restriction.
	allowed := r.Form["agent_ids"]
	// prior_ids: operator-selected prior investigations to attach (mirrors
	// agent_ids). Merged with the automatic host-scoped selection in Start.
	priorIDs := r.Form["prior_ids"]
	modelProfile := strings.TrimSpace(r.FormValue("model_profile"))
	if modelProfile != "" && !s.knownLLMProfile(modelProfile) {
		http.Error(w, "unknown model profile", http.StatusBadRequest)
		return
	}
	if len(priorIDs) > 0 {
		s.log.Debug("new investigation with operator-selected priors", "count", len(priorIDs))
	}
	id, err := s.loop.StartWithPriors(r.Context(), goal, authedUser(r), modelProfile, priorIDs, allowed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(r.Context(), authedUser(r), "investigation.start",
		map[string]any{"investigation_id": id, "goal_chars": len(goal), "allowed_hosts": len(allowed), "model_profile": modelProfile, "priors": len(priorIDs)})
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

func (s *Server) handleInvestigationsDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/investigations/")
	if rest == "" || rest == "new" || rest == "decide" {
		http.NotFound(w, r)
		return
	}
	id := rest
	data, err := s.investigationDetailData(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	data["ContinueFlash"] = s.popContinueFlash(r)
	s.renderForReq(w, r, "investigation_detail", data)
}

// timelineView returns tool calls newest-first (descending seq) for the
// operator-facing detail page. The store list (ListToolCalls) is ascending by
// seq and is walked tail-first by investigator gates — post-finding lockdown
// (restrict.go:41), the ask-streak and last-approved scans (loop.go), and the
// coverage/explanation gates — so the reversal lives ONLY here in the web view
// layer and never in the store. Returns a fresh slice; the input is untouched.
func timelineView(tcs []store.ToolCallRow) []store.ToolCallRow {
	out := make([]store.ToolCallRow, len(tcs))
	for i := range tcs {
		out[len(tcs)-1-i] = tcs[i]
	}
	return out
}

// findingsView returns findings newest-first WITHIN each pin/ignore group.
// The store (ListFindings) orders (pinned DESC, ignored ASC, created_at ASC);
// a naive full reversal would also flip the group order (ignored findings
// would float to the top), so this re-sorts a copy keeping the same group key
// and only flipping created_at to DESC. Web view layer only — ListFindings
// stays ascending for its order-independent investigator caller (priors.go).
func findingsView(findings []store.Finding) []store.Finding {
	out := make([]store.Finding, len(findings))
	copy(out, findings)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Pinned != b.Pinned {
			return a.Pinned // pinned group first
		}
		if a.Ignored != b.Ignored {
			return !a.Ignored // non-ignored before ignored
		}
		return a.CreatedAt.After(b.CreatedAt) // newest first within the group
	})
	return out
}

func (s *Server) investigationDetailData(ctx context.Context, id string) (map[string]any, error) {
	if s.loop != nil {
		s.loop.EnsureProgress(ctx, id, "detail_data")
	}
	inv, err := s.store.GetInvestigation(ctx, id)
	if err != nil {
		return nil, err
	}
	tcs, _ := s.store.ListToolCalls(ctx, id)
	findings, _ := s.store.ListFindings(ctx, id)
	// Operator-facing order: newest activity first. Reversed in the web view
	// layer only (see timelineView / findingsView) — the store lists stay
	// ascending for the tail-first investigator gate walks.
	tcsView := timelineView(tcs)
	findsView := findingsView(findings)
	memories, _ := s.store.ListMemory(ctx, id, 20)
	// Attached cross-investigation priors for the detail "Prior investigations"
	// panel (operator visibility). Static per run — recorded once at Start.
	var priorsAttached []priorChoice
	if len(inv.Priors) > 0 {
		pp, _ := s.store.ListInvestigationsByIDs(ctx, inv.Priors)
		priorsAttached = priorChoicesFrom(pp)
	}
	pending, _ := s.store.PendingToolCall(ctx, id)

	maxSteps, maxTokens := s.budgets()
	// Effective per-investigation cap = global default + per-inv extras the
	// operator bought after a budget-exhausted pause.
	maxSteps += inv.ExtraSteps
	maxTokens += inv.ExtraTokens
	usedTokens := inv.TotalPromptTokens + inv.TotalCompletionTokens
	stepsPct := safePct(inv.TotalToolCalls, maxSteps)
	tokensPct := safePct(usedTokens, maxTokens)
	initSnap, _ := s.snapshotForSSE(ctx, id)
	notebookAvailable := false
	if s.nb != nil {
		_, notebookAvailable = s.nb.Path(id)
	}
	terminal := s.terminalPayloadView(id, inv.SummaryJSON)
	canContinueAborted := inv.Status == "aborted" && s.availability.Enabled && (!terminal.Present || terminal.Recoverable)
	// Done investigations are reopenable in place (operator request): continue is
	// always available for a completed run when the investigator is configured —
	// recoverability does not apply to a clean completion.
	canContinueDone := inv.Status == "done" && s.availability.Enabled
	// A transient LLM failure (network / 5xx / rate-limit) is recovered by
	// re-sending the same request, so offer a one-click retry distinct from the
	// free-text continue flow.
	canRetryTransient := canContinueAborted && terminal.Present &&
		terminal.Kind == store.TerminalKindLLMError && terminal.Source == "llm" && terminal.Transient
	continuePlaceholder := continuePlaceholderForTerminal(terminal)
	if s.log != nil {
		s.log.Debug("build investigation detail data",
			"investigation_id", id,
			"investigator_enabled", s.availability.Enabled,
			"investigator_reason_class", s.availability.DisabledReason)
	}
	data := map[string]any{
		"Title":               "Investigation " + id,
		"Version":             version.Version,
		"Inv":                 inv,
		"ToolCalls":           tcsView,
		"Findings":            findsView,
		"Memories":            memories,
		"MemoryCount":         len(memories),
		"PriorsAttached":      priorsAttached,
		"NotebookAvailable":   notebookAvailable,
		"Pending":             pending,
		"Investigator":        s.availability,
		"LLMEnabled":          s.availability.Enabled,
		"MaxSteps":            maxSteps,
		"MaxTokens":           maxTokens,
		"UsedTokens":          usedTokens,
		"StepsPct":            stepsPct,
		"TokensPct":           tokensPct,
		"InitSnap":            initSnap,
		"Terminal":            terminal,
		"TerminalKind":        terminal.Kind,
		"TerminalReason":      terminal.Reason,
		"TerminalDetail":      terminal.Detail,
		"TerminalRecoverable": terminal.Recoverable,
		"TerminalSource":      terminal.Source,
		"CanContinueAborted":  canContinueAborted,
		"CanContinueDone":     canContinueDone,
		"CanRetryTransient":   canRetryTransient,
		"ContinuePlaceholder": continuePlaceholder,
	}
	// Per-region content fingerprints for the client swap gate (flicker fix):
	// each region is re-rendered client-side only when ITS own content changed,
	// so the token/budget churn in the status fragment no longer reflows the
	// timeline and side regions on every poll tick. Computed from the same data
	// that renders the page, so the initial-load hash and the poll hash agree.
	data["StatusHash"] = s.regionHash("investigation_status_fragment", data)
	data["TimelineHash"] = s.regionHash("investigation_timeline_fragment", data)
	// The side panel also displays "tokens used" / compaction counters that
	// churn every turn (duplicated live in the status budget bar). Hashing its
	// rendered HTML would reflow the findings list and collapse expanded
	// evidence on every poll tick, so the side region uses a STRUCTURED hash
	// over its discrete content with those volatile duplicates excluded.
	data["SideHash"] = sideRegionHash(inv.Status, len(tcs), len(memories), findsView)
	return data, nil
}

func (s *Server) terminalPayloadView(investigationID string, raw sql.NullString) terminalPayloadView {
	payload, ok := store.ParseInvestigationTerminalPayload(raw)
	if !ok {
		return terminalPayloadView{}
	}
	if s.log != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw.String), &obj); err != nil {
			s.log.Warn("parse terminal payload",
				"investigation_id", investigationID,
				"failure_class", "invalid_json",
				"payload_bytes", len(raw.String))
		} else if _, typed := obj["kind"]; !typed {
			s.log.Warn("parse terminal payload",
				"investigation_id", investigationID,
				"failure_class", "legacy_payload",
				"payload_bytes", len(raw.String))
		}
	}
	return terminalPayloadView{
		Present:     true,
		Kind:        payload.Kind,
		Reason:      payload.Reason,
		Detail:      payload.Detail,
		Recoverable: payload.Recoverable,
		Transient:   payload.Transient,
		Source:      payload.Source,
		Summary:     parseTerminalSummary(payload.Summary),
	}
}

// parseTerminalSummary decodes the structured mark_done conclusion embedded in
// the terminal payload. It returns nil — so the Conclusion card is skipped —
// when the payload carries no summary (legacy / non-done terminals) or when the
// summary has no operator-meaningful content, rather than rendering a blank
// shell. Tolerant of partial summaries: a clean done always has root_cause +
// recommended_remediation, but reopened or budget-finalized rows may not.
func parseTerminalSummary(raw json.RawMessage) *terminalSummaryView {
	if len(raw) == 0 {
		return nil
	}
	var s struct {
		RootCause              string   `json:"root_cause"`
		RootCauseExplains      string   `json:"root_cause_explains"`
		Confidence             string   `json:"confidence"`
		Symptoms               []string `json:"symptoms"`
		HostsExamined          []string `json:"hosts_examined"`
		EvidenceRefs           []string `json:"evidence_refs"`
		WhereToLookNext        []string `json:"where_to_look_next"`
		RecommendedRemediation string   `json:"recommended_remediation"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	if strings.TrimSpace(s.RootCause) == "" && strings.TrimSpace(s.RecommendedRemediation) == "" &&
		len(s.Symptoms) == 0 && len(s.EvidenceRefs) == 0 && len(s.WhereToLookNext) == 0 &&
		len(s.HostsExamined) == 0 {
		return nil
	}
	return &terminalSummaryView{
		RootCause:              strings.TrimSpace(s.RootCause),
		RootCauseExplains:      strings.TrimSpace(s.RootCauseExplains),
		Confidence:             strings.TrimSpace(s.Confidence),
		Symptoms:               s.Symptoms,
		HostsExamined:          s.HostsExamined,
		EvidenceRefs:           s.EvidenceRefs,
		WhereToLookNext:        s.WhereToLookNext,
		RecommendedRemediation: strings.TrimSpace(s.RecommendedRemediation),
	}
}

// confidenceBadge maps a mark_done confidence level to a badge CSS class so a
// speculative/inconclusive close never visually reads as a confirmed one.
func confidenceBadge(confidence string) string {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "confirmed":
		return "ok"
	case "likely":
		return "info"
	case "speculative":
		return "warn"
	case "inconclusive":
		return "pending"
	default:
		return "info"
	}
}

func continuePlaceholderForTerminal(terminal terminalPayloadView) string {
	if !terminal.Present || terminal.Reason == "" {
		return "Continue from the last good evidence. Retry the transient failure and finish the diagnosis."
	}
	return "Continue from the last good evidence. Address this abort reason first: " + terminal.Reason
}

func (s *Server) handleInvestigationFragments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/investigations/fragments/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	data, err := s.investigationDetailData(r.Context(), id)
	if err != nil {
		s.log.Warn("investigation fragment data", "investigation_id", id, "err", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	data["CSRF"] = s.csrfTokenFor(r)
	data["UserTZ"] = userLocation(r)
	w.Header().Set("Cache-Control", "no-store")
	s.renderFragment(w, "investigation_live_fragments", data)
}

// wantsFragment reports whether the caller is the in-page fetch() live engine
// (which sets X-Requested-With: fetch) rather than a plain browser form POST.
// The engine swaps the returned live fragments in place; a classic POST still
// gets the 303 redirect so the page works with JavaScript disabled. This is
// the progressive-enhancement seam for the no-reload approve loop.
func wantsFragment(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Requested-With"), "fetch")
}

// respondAction finishes an operator action handler (decide/hypothesis/
// retry/continue). For the in-page fetch() engine it renders the live
// fragments so the page advances without a reload — flash carries any
// rejection text into the rendered fragment via ContinueFlash. For a classic
// form POST it sets the session flash (if any) and 303-redirects to the detail
// page. Pass an empty flash on the success path.
func (s *Server) respondAction(w http.ResponseWriter, r *http.Request, id, flash string) {
	if wantsFragment(r) {
		s.writeLiveFragments(w, r, id, flash)
		return
	}
	if flash != "" {
		s.flashContinue(r, flash)
	}
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

// writeLiveFragments renders the live-update partial (status + timeline +
// side) for id as the AJAX response to an operator action — the fetch()
// counterpart of the 303 redirect. A non-empty flash is injected as
// ContinueFlash so a rejection (e.g. wrong status) shows in-place without a
// reload. On a data error it falls back to 204 so the client just re-fetches
// through its normal refresh path.
func (s *Server) writeLiveFragments(w http.ResponseWriter, r *http.Request, id, flash string) {
	data, err := s.investigationDetailData(r.Context(), id)
	if err != nil {
		s.log.Warn("live fragment response", "investigation_id", id, "err", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	data["CSRF"] = s.csrfTokenFor(r)
	data["UserTZ"] = userLocation(r)
	if flash != "" {
		data["ContinueFlash"] = flash
	}
	w.Header().Set("Cache-Control", "no-store")
	s.log.Debug("[FIX:investigation-live] ajax action fragment",
		"investigation_id", id, "rejected", flash != "")
	s.renderFragment(w, "investigation_live_fragments", data)
}

// budgets returns the configured per-investigation budgets so the UI can
// render a usage bar. When the loop is not configured we fall back to plan
// defaults.
func (s *Server) budgets() (steps, tokens int) {
	if s.loop != nil {
		steps, tokens = s.loop.Budgets()
	}
	if steps == 0 {
		steps = 40
	}
	if tokens == 0 {
		tokens = 500_000
	}
	return
}

func safePct(used, max int) int {
	if max <= 0 {
		return 0
	}
	p := used * 100 / max
	if p > 100 {
		p = 100
	}
	return p
}

func (s *Server) investigatorDisabledMessage() string {
	if s.availability.ConfigHint != "" {
		return "investigator disabled — " + s.availability.ConfigHint
	}
	return "investigator disabled"
}

func (s *Server) handleInvestigationDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, s.investigatorDisabledMessage(), http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("investigation_id")
	decision := r.FormValue("decision")
	if id == "" || decision == "" {
		http.Error(w, "investigation_id and decision required", http.StatusBadRequest)
		return
	}
	newInput := r.FormValue("new_input_json")
	if err := s.loop.DecideWithEdit(r.Context(), id, decision, newInput, "operator"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(r.Context(), "operator", "investigation.decide",
		map[string]any{"investigation_id": id, "decision": decision, "edited": newInput != ""})
	s.respondAction(w, r, id, "")
}

// handleInvestigationAnswer delivers the operator's answer to a pending
// ask_operator question (the model's question) back to the model as that tool
// call's result, then resumes the loop. Mirrors handleInvestigationDecide's
// content negotiation via respondAction so it works with the no-reload engine.
func (s *Server) handleInvestigationAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, s.investigatorDisabledMessage(), http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("investigation_id"))
	toolCallID := strings.TrimSpace(r.FormValue("tool_call_id"))
	answer := strings.TrimSpace(r.FormValue("answer"))
	if id == "" || toolCallID == "" {
		http.Error(w, "investigation_id and tool_call_id required", http.StatusBadRequest)
		return
	}
	if answer == "" {
		s.respondAction(w, r, id, "Type an answer before sending it to the model.")
		return
	}
	if err := s.loop.AnswerOperator(r.Context(), id, toolCallID, answer, authedUser(r)); err != nil {
		s.log.Info("answer operator rejected", "investigation_id", id, "err", sanitizeOperatorError(err))
		s.respondAction(w, r, id, "Could not deliver answer: "+sanitizeOperatorError(err))
		return
	}
	s.audit(r.Context(), authedUser(r), "investigation.answer_operator",
		map[string]any{"investigation_id": id, "tool_call_id": toolCallID, "answer_chars": len(answer)})
	s.respondAction(w, r, id, "")
}

func (s *Server) handleHypothesis(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, s.investigatorDisabledMessage(), http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("investigation_id")
	claim := r.FormValue("claim")
	expected := r.FormValue("expected")
	instruction := r.FormValue("instruction")
	if id == "" || strings.TrimSpace(claim) == "" {
		http.Error(w, "investigation_id and claim required", http.StatusBadRequest)
		return
	}
	if err := s.loop.InjectHypothesis(r.Context(), id, claim, expected, instruction, "operator"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.audit(r.Context(), "operator", "investigation.hypothesis",
		map[string]any{"investigation_id": id, "claim_chars": len(claim)})
	s.respondAction(w, r, id, "")
}

// handleInvestigationContinue reopens an aborted investigation with an
// operator message. This is intentionally separate from the hypothesis form:
// it is a recovery action for terminal aborts, not a normal redirect of an
// active run.
func (s *Server) handleInvestigationContinue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if err := r.ParseForm(); err != nil {
		s.log.Warn("continue form parse failed", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("investigation_id"))
	message := strings.TrimSpace(r.FormValue("message"))
	if id == "" {
		http.Error(w, "investigation_id required", http.StatusBadRequest)
		return
	}
	if !s.availability.Enabled {
		s.log.Info("continue investigation rejected",
			"investigation_id", id,
			"failure_class", "disabled",
			"reason_class", s.availability.DisabledReason,
			"message_chars", len(message))
		s.respondAction(w, r, id, "Continuation requires a configured LLM client and hub restart. "+s.availability.ConfigHint)
		return
	}
	inv, err := s.store.GetInvestigation(r.Context(), id)
	if err != nil {
		s.log.Warn("continue investigation lookup failed", "investigation_id", id, "err", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if message == "" {
		s.log.Info("continue investigation rejected", "investigation_id", id, "status", inv.Status, "failure_class", "empty_message")
		s.respondAction(w, r, id, "Add an operator message before continuing this investigation.")
		return
	}
	// Aborted runs and completed (done) runs are both reopenable in place; every
	// other status (active/waiting/paused) is not a continuation target.
	if inv.Status != "aborted" && inv.Status != "done" {
		s.log.Info("continue investigation rejected", "investigation_id", id, "status", inv.Status, "failure_class", "wrong_status")
		s.respondAction(w, r, id, "Only aborted or completed investigations can be continued. Current status: "+inv.Status+".")
		return
	}
	if err := s.loop.ResumeAborted(r.Context(), id, message, authedUser(r)); err != nil {
		s.log.Info("continue investigation rejected",
			"investigation_id", id,
			"status", inv.Status,
			"failure_class", "resume_error",
			"err", sanitizeOperatorError(err),
			"message_chars", len(message))
		s.respondAction(w, r, id, "Could not continue investigation: "+sanitizeOperatorError(err))
		return
	}
	s.audit(r.Context(), authedUser(r), "investigation.resume_aborted",
		map[string]any{"investigation_id": id, "message_chars": len(message)})
	s.respondAction(w, r, id, "")
}

// handleInvestigationRetry re-sends the same last request for a transient LLM
// abort — no operator message required. It mirrors handleInvestigationContinue
// minus the message, and is the one-click recovery for "the POST to the LLM
// failed; just send it again". Non-transient aborts still use the free-text
// continue flow.
func (s *Server) handleInvestigationRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		s.log.Warn("retry form parse failed", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("investigation_id"))
	if id == "" {
		http.Error(w, "investigation_id required", http.StatusBadRequest)
		return
	}
	if !s.availability.Enabled {
		s.log.Info("retry investigation rejected",
			"investigation_id", id, "failure_class", "disabled", "reason_class", s.availability.DisabledReason)
		s.respondAction(w, r, id, "Retry requires a configured LLM client and hub restart. "+s.availability.ConfigHint)
		return
	}
	inv, err := s.store.GetInvestigation(r.Context(), id)
	if err != nil {
		s.log.Warn("retry investigation lookup failed", "investigation_id", id, "err", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if inv.Status != "aborted" {
		s.log.Info("retry investigation rejected", "investigation_id", id, "status", inv.Status, "failure_class", "wrong_status")
		msg := "Only aborted investigations can be retried. Current status: " + inv.Status + "."
		if inv.Status == "done" {
			msg = "Done investigations are terminal and cannot be retried."
		}
		s.respondAction(w, r, id, msg)
		return
	}
	if err := s.loop.RetryLastStep(r.Context(), id, authedUser(r)); err != nil {
		s.log.Info("retry investigation rejected",
			"investigation_id", id, "status", inv.Status, "failure_class", "retry_error",
			"err", sanitizeOperatorError(err))
		s.respondAction(w, r, id, "Could not retry investigation: "+sanitizeOperatorError(err))
		return
	}
	s.audit(r.Context(), authedUser(r), "investigation.retry_transient",
		map[string]any{"investigation_id": id})
	s.respondAction(w, r, id, "")
}

func (s *Server) flashContinue(r *http.Request, msg string) {
	if sid, err := r.Cookie(cookieSession); err == nil && sid.Value != "" {
		s.sessions.setFlash(sid.Value, "continue_error", msg)
	}
}

func (s *Server) popContinueFlash(r *http.Request) string {
	if sid, err := r.Cookie(cookieSession); err == nil && sid.Value != "" {
		return s.sessions.popFlash(sid.Value, "continue_error")
	}
	return ""
}

func sanitizeOperatorError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(msg) > 240 {
		msg = msg[:240] + "..."
	}
	return msg
}

// handleInvestigationExtend bumps the per-investigation budget extras and
// resumes the paused loop. Form fields: investigation_id (required),
// extra_steps (optional, default +5), extra_tokens (optional, default
// +500_000). One click of "Add 500k tokens" → +500K with a small step
// nudge so the model isn't immediately re-paused on step cap.
func (s *Server) handleInvestigationExtend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, s.investigatorDisabledMessage(), http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("investigation_id")
	if id == "" {
		http.Error(w, "investigation_id required", http.StatusBadRequest)
		return
	}
	extraSteps := 10
	if v := r.FormValue("extra_steps"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 200 {
			extraSteps = n
		}
	}
	extraTokens := 500_000
	if v := r.FormValue("extra_tokens"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 2_000_000 {
			extraTokens = n
		}
	}
	if err := s.loop.Extend(r.Context(), id, extraSteps, extraTokens, authedUser(r)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

// handleInvestigationFinalize tells the loop to emit a closing mark_done
// with whatever evidence is on the timeline + "where to look next"
// hypotheses, even though the budget is exhausted.
func (s *Server) handleInvestigationFinalize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, s.investigatorDisabledMessage(), http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("investigation_id")
	if id == "" {
		http.Error(w, "investigation_id required", http.StatusBadRequest)
		return
	}
	if err := s.loop.Finalize(r.Context(), id, authedUser(r)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

// handleInvestigationAutoApprove flips the per-investigation auto_approve
// toggle. Form fields: investigation_id, on=1|0. When toggled on with a
// pending tool_call already on the timeline we DO NOT auto-execute it —
// operator decides on the in-flight one explicitly; auto-approval applies
// only to subsequent calls.
func (s *Server) handleInvestigationAutoApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("investigation_id")
	if id == "" {
		http.Error(w, "investigation_id required", http.StatusBadRequest)
		return
	}
	on := r.FormValue("on") == "1"
	if err := s.store.SetAutoApprove(r.Context(), id, on); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), authedUser(r), "investigation.auto_approve",
		map[string]any{"investigation_id": id, "on": on})
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

// handleInvestigationAutonomous arms or disarms a bounded autonomous run. Form
// fields: investigation_id, action=arm|disarm, and (for arm) steps + tokens
// deltas. Arming auto-approves probe tool_calls (no per-step confirmation) until
// the delta is spent, then pauses for review; disarming hands control back to
// step-by-step approval. Mirrors handleInvestigationExtend (redirect, no live
// fragments). CSRF is enforced by the auth middleware.
func (s *Server) handleInvestigationAutonomous(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.loop == nil {
		http.Error(w, s.investigatorDisabledMessage(), http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.FormValue("investigation_id")
	if id == "" {
		http.Error(w, "investigation_id required", http.StatusBadRequest)
		return
	}
	switch r.FormValue("action") {
	case "disarm":
		if err := s.loop.DisarmAutonomousRun(r.Context(), id, authedUser(r)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	case "arm", "":
		steps := 0
		if v := r.FormValue("steps"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 500 {
				steps = n
			}
		}
		tokens := 0
		if v := r.FormValue("tokens"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 5_000_000 {
				tokens = n
			}
		}
		if err := s.loop.StartAutonomousRun(r.Context(), id, steps, tokens, authedUser(r)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "action must be arm|disarm", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/investigations/"+id, http.StatusSeeOther)
}

func (s *Server) handleFindingAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/findings/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "expected /findings/{id}/{action}", http.StatusBadRequest)
		return
	}
	id, action := parts[0], parts[1]
	f, err := s.store.GetFinding(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	switch action {
	case "pin":
		err = s.store.SetFindingPinned(r.Context(), id, true)
	case "unpin":
		err = s.store.SetFindingPinned(r.Context(), id, false)
	case "ignore":
		// (review M3) Idempotent — re-ignoring an already-ignored finding
		// must not stack duplicate system_notes in the message stream.
		if f.Ignored {
			http.Redirect(w, r, "/investigations/"+f.InvestigationID, http.StatusSeeOther)
			return
		}
		err = s.store.SetFindingIgnored(r.Context(), id, true)
		if err == nil && s.loop != nil {
			_ = s.loop.InjectIgnoreNote(r.Context(), f.InvestigationID, f.Code, f.Message)
		}
	case "unignore":
		// (review M4) Idempotent + emit a restore note so the model sees
		// the IGNORED directive being lifted; otherwise the older "do not
		// investigate" note hangs in context unrebutted.
		if !f.Ignored {
			http.Redirect(w, r, "/investigations/"+f.InvestigationID, http.StatusSeeOther)
			return
		}
		err = s.store.SetFindingIgnored(r.Context(), id, false)
		if err == nil && s.loop != nil {
			_ = s.loop.InjectRestoreNote(r.Context(), f.InvestigationID, f.Code, f.Message)
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), "operator", "finding."+action,
		map[string]any{"finding_id": id, "investigation_id": f.InvestigationID})
	http.Redirect(w, r, "/investigations/"+f.InvestigationID, http.StatusSeeOther)
}

func (s *Server) handleInvestigationExport(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/investigations/export/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	inv, err := s.store.GetInvestigation(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// The Markdown export stays in ascending store order (oldest-first) on
	// purpose: a document reads top-to-bottom chronologically and a stable order
	// keeps re-exported diffs small. Only the live web detail page is reversed to
	// newest-first (see investigationDetailData / timelineView / findingsView).
	tcs, _ := s.store.ListToolCalls(r.Context(), id)
	findings, _ := s.store.ListFindings(r.Context(), id)
	memories, _ := s.store.ListMemory(r.Context(), id, 200)

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.md"`)
	_, _ = fmt.Fprintf(w, "# Investigation %s\n\n", inv.ID)
	_, _ = fmt.Fprintf(w, "- **Status:** %s\n- **Model:** %s\n- **Created:** %s\n- **Steps:** %d\n- **Tokens:** %d prompt + %d completion\n",
		inv.Status, inv.Model, inv.CreatedAt.UTC().Format(time.RFC3339),
		inv.TotalToolCalls, inv.TotalPromptTokens, inv.TotalCompletionTokens)
	if s.nb != nil {
		if _, ok := s.nb.Path(id); ok {
			_, _ = fmt.Fprintf(w, "- **Notebook:** [download](/investigations/notebook/%s?download=1)\n", id)
		}
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "## Goal\n\n> %s\n\n", inv.Goal)

	if terminal := s.terminalPayloadView(id, inv.SummaryJSON); terminal.Present {
		heading := "Terminal summary"
		if inv.Status == "aborted" {
			heading = "Abort reason"
		}
		_, _ = fmt.Fprintf(w, "## %s\n\n", heading)
		_, _ = fmt.Fprintf(w, "- **Kind:** `%s`\n- **Reason:** %s\n- **Recoverable:** `%t`\n- **Source:** `%s`\n",
			terminal.Kind, terminal.Reason, terminal.Recoverable, terminal.Source)
		if terminal.Detail != "" {
			// Terminal detail is sanitized and length-capped by store.NewInvestigationTerminalPayload
			// and the legacy parser before reaching export.
			_, _ = fmt.Fprintf(w, "- **Detail:** %s\n", terminal.Detail)
		}
		_, _ = fmt.Fprintln(w)
	}

	_, _ = fmt.Fprintf(w, "## Findings\n\n")
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "_(none)_")
	}
	for _, f := range findings {
		mark := ""
		if f.Pinned {
			mark = " 📌"
		}
		if f.Ignored {
			mark = " 🚫"
		}
		_, _ = fmt.Fprintf(w, "- **[%s]** `%s`%s — %s\n", strings.ToUpper(f.Severity), f.Code, mark, f.Message)
	}

	if digest := investigator.RenderMemoryDigest(memories); digest != "" {
		_, _ = fmt.Fprintf(w, "\n## Durable memory\n\n%s", digest)
	}

	_, _ = fmt.Fprintf(w, "\n## Tool-call timeline\n\n")
	for _, tc := range tcs {
		_, _ = fmt.Fprintf(w, "### %d. `%s` — _%s_\n", tc.Seq, tc.Tool, tc.Status)
		if tc.Rationale != "" {
			_, _ = fmt.Fprintf(w, "> %s\n\n", tc.Rationale)
		}
		// (review M9) Use 4-tilde fences instead of triple-backtick so JSON
		// content containing literal ``` doesn't break the rendered .md.
		_, _ = fmt.Fprintf(w, "**Input:**\n~~~~json\n%s\n~~~~\n", prettyJSON([]byte(tc.InputJSON)))
		if tc.ResultJSON.Valid && tc.ResultJSON.String != "" {
			_, _ = fmt.Fprintf(w, "**Result:**\n~~~~json\n%s\n~~~~\n", prettyJSON([]byte(tc.ResultJSON.String)))
		}
		_, _ = fmt.Fprintln(w)
	}

	if inv.SummaryJSON.Valid {
		_, _ = fmt.Fprintf(w, "## Summary\n\n~~~~json\n%s\n~~~~\n", prettyJSON([]byte(inv.SummaryJSON.String)))
	}
}

// handleInvestigationNotebook serves the investigation's notebook.md for
// inline viewing or download. The path is validated inside the Notebook
// (NotebookPath) so it cannot escape the artifact root.
func (s *Server) handleInvestigationNotebook(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/investigations/notebook/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if s.nb == nil {
		http.NotFound(w, r)
		return
	}
	body, err := s.nb.Read(id, 4*1024*1024)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+id+`-notebook.md"`)
	}
	_, _ = w.Write(body)
}

// handleInvestigationSSE streams a minimal status pulse to the browser. We do
// NOT stream LLM chunks here — the loop is poll-based, so SSE just announces
// server-side state transitions and the page fetches bounded HTML fragments.
func (s *Server) handleInvestigationSSE(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/investigations/events/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx friendly

	s.log.Debug("investigation sse open", "investigation_id", id)
	defer s.log.Debug("investigation sse closed", "investigation_id", id)

	// Push-based channel: subscribe to the investigator Bus so a state change
	// reaches the client the instant it is published, instead of waiting for
	// the next poll tick. When the LLM is disabled (loop == nil) there are no
	// Bus events; busCh stays nil — a nil channel blocks forever in select, so
	// the safety re-snapshot ticker becomes the sole updater.
	var busCh <-chan investigator.Event
	if s.loop != nil {
		ch, unsubscribe := s.loop.Bus().Subscribe(id)
		defer unsubscribe()
		busCh = ch
	}

	// emit re-snapshots and tracks the last fingerprint already sent. It only
	// writes a `state-change` when the server fingerprint actually changed
	// (the client gates DOM swaps on it too); otherwise it writes a heartbeat
	// comment so the connection does not idle-close at the proxy. Returns
	// false when the connection should be torn down (snapshot error).
	last := ""
	emit := func(reason string) bool {
		snap, err := s.snapshotForSSE(r.Context(), id)
		if err != nil {
			s.log.Warn("investigation sse snapshot", "investigation_id", id, "reason", reason, "err", err)
			return false
		}
		if snap == last {
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
			return true
		}
		last = snap
		s.log.Debug("[FIX:investigation-live] sse state-change",
			"investigation_id", id, "reason", reason)
		//nolint:gosec // G705: SSE response is text/event-stream, not HTML — no XSS surface; snap is %q-quoted JSON.
		_, _ = fmt.Fprintf(w, "event: state-change\ndata: %s\n\n", snap)
		flusher.Flush()
		return true
	}

	// Initial snapshot on connect: lets a freshly (re)connected client resync
	// immediately without waiting for the first Bus event or poll tick. This is
	// the backstop that self-heals a refresh dropped while a previous fetch was
	// still in flight — the now-final state is always re-sent on (re)connect.
	if !emit("initial") {
		return
	}

	safety := time.NewTicker(10 * time.Second) // re-snapshot backstop if a Bus event is ever coalesced/dropped
	defer safety.Stop()
	deadline := time.After(5 * time.Minute) // cap connection life
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			s.log.Info("[FIX:investigation-live] reconnecting long-lived investigation SSE",
				"investigation_id", id)
			_, _ = fmt.Fprint(w, "event: bye\ndata: timeout\n\n")
			flusher.Flush()
			return
		case _, open := <-busCh:
			if !open {
				// Bus subscription closed (hub shutdown). Stop selecting on it;
				// the safety ticker keeps the page fresh until the client
				// disconnects or the deadline fires.
				busCh = nil
				continue
			}
			if !emit("bus") {
				return
			}
		case <-safety.C:
			if !emit("safety") {
				return
			}
		}
	}
}

// snapshotForSSE returns a small JSON digest used to detect whether the
// page should self-refresh: status, tool_call count, latest tool_call
// status, findings count. Single SQL query (review M8).
func (s *Server) snapshotForSSE(ctx context.Context, id string) (string, error) {
	if s.loop != nil {
		s.loop.EnsureProgress(ctx, id, "sse_snapshot")
	}
	status, last, steps, findings, updatedAt, promptTokens, completionTokens, totalToolCalls, extraSteps, extraTokens, terminalSummary, err := s.store.SnapshotCounters(ctx, id)
	if err != nil {
		return "", err
	}
	terminalHash := ""
	if terminalSummary.Valid {
		sum := sha256.Sum256([]byte(terminalSummary.String))
		terminalHash = fmt.Sprintf("%x", sum[:])
	}
	return fmt.Sprintf(`{"status":%q,"steps":%d,"last":%q,"findings":%d,"updated_at":%q,"used_tokens":%d,"total_tool_calls":%d,"extra_steps":%d,"extra_tokens":%d,"terminal":%q}`,
		status, steps, last, findings, updatedAt.UTC().Format(time.RFC3339Nano), promptTokens+completionTokens, totalToolCalls, extraSteps, extraTokens, terminalHash), nil
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	actor := r.URL.Query().Get("actor")
	action := r.URL.Query().Get("action")
	entries, err := s.store.ListAuditFiltered(r.Context(), actor, action, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderForReq(w, r, "audit", map[string]any{
		"Title":   "Audit",
		"Version": version.Version,
		"Entries": entries,
		"Actor":   actor,
		"Action":  action,
	})
}
