package web

import (
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// installScriptTemplate is the bash one-liner the install URL serves. The
// hub fills in the four placeholders before sending. The script is
// idempotent enough to be re-run by hand for debugging — every step skips
// when the target already exists.
//
//go:embed install_agent.sh.tpl
var installScriptTemplate string

// handleInstallAgentScript serves the templated install script. No session
// auth — the bootstrap token in the URL IS the authentication. Anyone with
// the URL can enrol an agent under the bound agent_id ONCE; tokens are
// single-use (see store.ConsumeBootstrapToken).
//
// Required query params:
//
//	token   bootstrap token (single-use, agent-bound)
//	id      agent_id the token was issued for
//
// Optional:
//
//	hub     host:port the agent talks to (overrides hub.yaml install.agent_grpc_endpoint)
//	version release tag to download (overrides hub.yaml install.version)
func (s *Server) handleInstallAgentScript(w http.ResponseWriter, r *http.Request) {
	if !s.install.Enabled() {
		http.Error(w, "install endpoint is disabled — set install.download_base_url and install.agent_grpc_endpoint in hub.yaml", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	token := q.Get("token")
	agentID := q.Get("id")
	if token == "" || agentID == "" {
		http.Error(w, "token and id are required query params", http.StatusBadRequest)
		return
	}
	// Validate the (token, agent_id) pair without consuming the token —
	// stops unauthenticated probes from harvesting the script body
	// (which leaks DownloadBase, hub endpoint, agent_id naming, etc.).
	// Real agents proceed normally; the actual consume happens at Enroll.
	ok, err := s.store.LookupBootstrapToken(r.Context(), token, agentID)
	if err != nil || !ok {
		// Returning 404 (not 401/403) keeps the surface from
		// distinguishing "endpoint exists" from "wrong token", same
		// shape an attacker would see for a bogus path.
		http.NotFound(w, r)
		return
	}
	hubEP := q.Get("hub")
	if hubEP == "" {
		hubEP = s.install.AgentGRPCEndpoint
	}
	// Auto-derive when the operator left agent_grpc_endpoint at the magic
	// value "auto": take the hostname from the request (honouring nginx
	// X-Forwarded-Host) and append the configured grpc port. Lets the
	// install URL work from any host that can resolve the same name as
	// the operator's browser, without baking a deployment-specific
	// hostname into hub.yaml.
	if hubEP == "" || hubEP == "auto" {
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		// Strip port from the request host — we want only the hostname.
		if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i+1:], "]") {
			host = host[:i]
		}
		hubEP = fmt.Sprintf("%s:%d", host, s.install.GRPCPort)
	}
	// Per-request version override: the operator can pin a specific release
	// without changing hub.yaml by passing ?version=v0.2.3 in the URL.
	cfg := s.install
	if v := q.Get("version"); v != "" {
		cfg.Version = v
	}
	body := strings.NewReplacer(
		"__TOKEN__", shellQuote(token),
		"__AGENT_ID__", shellQuote(agentID),
		"__HUB_ENDPOINT__", shellQuote(hubEP),
		"__VERSION__", shellQuote(cfg.Version),
		"__DOWNLOAD_BASE__", shellQuote(cfg.DownloadBase()),
		"__RELEASE_REPO__", shellQuote(cfg.ReleaseRepoURL),
	).Replace(installScriptTemplate)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	//nolint:gosec // G705: not HTML — Content-Type=text/plain + nosniff makes
	//              the body uninterpretable as a script in any browser. Both
	//              user-controlled inputs are also shellQuote()-escaped for
	//              shell safety, which is the only consumer of this body.
	_, _ = w.Write([]byte(body))
}

// handleQuickInstall is the operator-side flow. Reads agent_id + ttl from
// the form, issues a bootstrap token bound to that id, then flashes the
// installer one-liner back to the /hosts page.
func (s *Server) handleQuickInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.install.Enabled() {
		http.Error(w, "install endpoint is disabled — set install.download_base_url and install.agent_grpc_endpoint in hub.yaml", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentID := strings.TrimSpace(r.FormValue("agent_id"))
	if agentID == "" {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	ttl := 1 * time.Hour
	if v := r.FormValue("ttl"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 && d <= 30*24*time.Hour {
			ttl = d
		}
	}
	// Quick install for an agent_id we've seen before is the operator's
	// "wipe and reinstall this host" lever — revoke the prior identity so
	// the install script's fresh enroll attempt isn't refused with
	// AlreadyExists. Refuse to do it silently when the host is currently
	// online (review M1) — operator must explicitly revoke from /hosts/<id>
	// or pick a different agent_id, otherwise the next gRPC poll from
	// the live agent would suddenly fail with no warning.
	if host, err := s.store.GetHost(r.Context(), agentID); err == nil && host.Status == "online" {
		http.Error(w, "agent_id "+agentID+" is currently online — revoke its cert from /hosts/"+agentID+" first, or pick a different agent_id", http.StatusConflict)
		return
	}
	if err := s.store.RevokeIdentity(r.Context(), agentID, "quick-install reissue"); err == nil {
		// Audit only when we actually replaced an enrolment row — keeps
		// brand-new installs from polluting the audit log with
		// no-op revokes.
		s.audit(r.Context(), authedUser(r), "identity.revoke",
			map[string]any{"agent_id": agentID, "reason": "quick-install reissue"})
	}

	tok, err := investigatorTokenFor(r.Context(), s, agentID, ttl, authedUser(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Base URL the install script will be fetched from. Priority:
	// 1. install.external_url in hub.yaml — set this when the operator
	//    browser path and the agent network path differ (orbstack on :443
	//    vs nginx exposed on :8443, etc).
	// 2. X-Forwarded-Host + X-Forwarded-Proto from nginx (depends on
	//    nginx config preserving the port via $http_host).
	// 3. r.Host + r.TLS — the bare loopback fallback.
	base := strings.TrimRight(s.install.ExternalURL, "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		base = scheme + "://" + host
	}
	scriptURL := fmt.Sprintf("%s/install/agent.sh?token=%s&id=%s", base, urlEscape(tok), urlEscape(agentID))
	// curl flags depend on whether the hub is fronted by a CA-trusted
	// cert. Default config is self-signed (`make compose-up`), so we
	// fall back to -k. Once the operator sets install.trusted_tls=true
	// (or RECON_INSTALL_TRUSTED_TLS=true) the install fetch is verified
	// — closes the MITM-substitutes-script-on-curl-pull surface (M2).
	flags := "-fsSL"
	if !s.install.TrustedTLS {
		flags = "-fsSLk"
	}
	oneLiner := fmt.Sprintf(`curl %s %q | sudo bash`, flags, scriptURL)

	s.audit(r.Context(), authedUser(r), "install.token_issued",
		map[string]any{"agent_id": agentID, "ttl": ttl.String()})

	if sid, err := r.Cookie(cookieSession); err == nil && sid != nil {
		s.sessions.setFlash(sid.Value, "install_one_liner", oneLiner)
		s.sessions.setFlash(sid.Value, "install_agent_id", agentID)
	}
	http.Redirect(w, r, "/hosts", http.StatusSeeOther)
}

// shellQuote escapes a value for safe interpolation inside a double-quoted
// shell string. Shells expand `$`, “ ` “, `"`, and `\` inside double
// quotes — escape each.
func shellQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	return r.Replace(s)
}

// urlEscape escapes a value for safe inclusion in a URL query string. We
// keep it minimal — bootstrap tokens are URL-safe base64 already and
// agent_ids are validated by the issue path. Replace anything else just in
// case.
func urlEscape(s string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
