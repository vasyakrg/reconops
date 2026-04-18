package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/vasyakrg/recon/internal/common/version"
)

// authConfig is what the hub passes to the web layer at construction. The
// password hash is bcrypt of the operator's plaintext password and is read
// from env at hub startup so it never lands on disk in plaintext.
type authConfig struct {
	Username     string // single operator for MVP
	PasswordHash string // bcrypt
	SessionTTL   time.Duration
	// BehindTLSProxy=true forces Secure-cookie on even when r.TLS is nil
	// — the typical prod topology terminates TLS at nginx and proxies
	// cleartext to the hub on loopback (review H4).
	BehindTLSProxy bool
}

func (c authConfig) Enabled() bool {
	return c.Username != "" && c.PasswordHash != ""
}

// session is a server-side record indexed by an opaque cookie id.
type session struct {
	username string
	csrf     string // 32-byte hex; double-submit token
	expires  time.Time

	// flash holds short-lived one-shot data attached to the next render
	// (e.g. a freshly-issued bootstrap token). Read once, then cleared.
	// (review C1: never put secrets in URL query — nginx access_log etc.)
	flashMu sync.Mutex
	flash   map[string]string
}

type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*session

	// loginAttempts is a per-key sliding window of failed login attempts;
	// keyed by client IP. Used by /login to throttle brute-force.
	// (review H1)
	attemptMu     sync.Mutex
	loginFailures map[string][]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		sessions:      map[string]*session{},
		loginFailures: map[string][]time.Time{},
	}
}

// loginAllowed reports whether a fresh login attempt from key (client IP)
// is permitted. Window: 10 failures / 5 minutes / IP. Caller invokes
// recordLoginFailure on bad credentials.
func (s *sessionStore) loginAllowed(key string) bool {
	s.attemptMu.Lock()
	defer s.attemptMu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	keep := s.loginFailures[key][:0]
	for _, t := range s.loginFailures[key] {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	s.loginFailures[key] = keep
	return len(keep) < 10
}

func (s *sessionStore) recordLoginFailure(key string) {
	s.attemptMu.Lock()
	defer s.attemptMu.Unlock()
	s.loginFailures[key] = append(s.loginFailures[key], time.Now())
}

func (s *sessionStore) issue(username string, ttl time.Duration) (sessionID, csrf string) {
	sid := randomToken()
	tok := randomToken()
	s.mu.Lock()
	s.sessions[sid] = &session{
		username: username, csrf: tok, expires: time.Now().Add(ttl),
		flash: map[string]string{},
	}
	s.mu.Unlock()
	return sid, tok
}

// setFlash attaches a one-shot value to the session.
func (s *sessionStore) setFlash(sid, key, val string) {
	s.mu.RLock()
	sess := s.sessions[sid]
	s.mu.RUnlock()
	if sess == nil {
		return
	}
	sess.flashMu.Lock()
	defer sess.flashMu.Unlock()
	if sess.flash == nil {
		sess.flash = map[string]string{}
	}
	sess.flash[key] = val
}

// popFlash returns and clears a one-shot value.
func (s *sessionStore) popFlash(sid, key string) string {
	s.mu.RLock()
	sess := s.sessions[sid]
	s.mu.RUnlock()
	if sess == nil {
		return ""
	}
	sess.flashMu.Lock()
	defer sess.flashMu.Unlock()
	v := sess.flash[key]
	delete(sess.flash, key)
	return v
}

func (s *sessionStore) lookup(sid string) *session {
	if sid == "" {
		return nil
	}
	s.mu.RLock()
	sess, ok := s.sessions[sid]
	s.mu.RUnlock()
	if !ok || time.Now().After(sess.expires) {
		return nil
	}
	return sess
}

func (s *sessionStore) revoke(sid string) {
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()
}

// gcExpired drops expired sessions periodically — bounded growth.
func (s *sessionStore) gcExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.expires) {
			delete(s.sessions, k)
		}
	}
}

func randomToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// investigatorTokenFor issues a bootstrap token bound to agentID. Lives in
// the web layer so the Settings handler can call it without a separate
// service object — it is a thin wrapper around store + auth helpers.
func investigatorTokenFor(ctx context.Context, s *Server, agentID string, ttl time.Duration, issuer string) (string, error) {
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := s.store.InsertBootstrapToken(ctx, tok, agentID, issuer, ttl); err != nil {
		return "", err
	}
	return tok, nil
}

func generateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// PasswordHashFromPlaintext is a CLI helper exposed in cmd/hub for
// generating the bcrypt hash an operator pastes into env / yaml.
func PasswordHashFromPlaintext(pw string) (string, error) {
	if len(pw) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(h), nil
}

// verifyPassword decodes the stored hash and compares.
func verifyPassword(pw, encodedHash string) bool {
	h, err := base64.RawStdEncoding.DecodeString(encodedHash)
	if err != nil {
		return false
	}
	return bcrypt.CompareHashAndPassword(h, []byte(pw)) == nil
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

const (
	cookieSession = "recon_sid"
	cookieCSRF    = "recon_csrf"
	formCSRF      = "csrf"
	headerCSRF    = "X-CSRF-Token"
)

// requireAuth wraps a handler. Behaviour:
//   - if auth is not configured (no admin password), the handler runs as-is
//     (single-trust loopback mode, see PROJECT.md §9.5 / config doc)
//   - otherwise unauthenticated requests redirect to /login (GET) or 401
//     (POST/everything else)
//   - authenticated requests get user info via context key authedUserKey
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Enabled() {
			h(w, r)
			return
		}
		sid, _ := r.Cookie(cookieSession)
		var sess *session
		if sid != nil {
			sess = s.sessions.lookup(sid.Value)
		}
		if sess == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusFound)
			} else {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
			return
		}
		// CSRF check on state-changing requests.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			provided := r.Header.Get(headerCSRF)
			if provided == "" {
				// Cap form body BEFORE ParseForm — gosec G120.
				r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
				_ = r.ParseForm()
				provided = r.FormValue(formCSRF)
			}
			cookieTok, _ := r.Cookie(cookieCSRF)
			if cookieTok == nil || provided == "" ||
				subtle.ConstantTimeCompare([]byte(cookieTok.Value), []byte(sess.csrf)) != 1 ||
				subtle.ConstantTimeCompare([]byte(provided), []byte(sess.csrf)) != 1 {
				http.Error(w, "csrf token mismatch", http.StatusForbidden)
				return
			}
		}
		ctx := context.WithValue(r.Context(), authedUserKey{}, sess.username)
		h(w, r.WithContext(ctx))
	}
}

type authedUserKey struct{}

// authedUser returns the authenticated user, or "operator" if auth is not
// configured (for backward-compat with single-trust mode).
func authedUser(r *http.Request) string {
	if v, ok := r.Context().Value(authedUserKey{}).(string); ok && v != "" {
		return v
	}
	return "operator"
}

// clientIP extracts the originating client address. Honors X-Forwarded-For
// (necessary behind nginx — without it the brute-force counter collapses
// onto a single key for every requester).
func clientIP(r *http.Request) string {
	// Caller is responsible for passing through only requests they trust
	// XFF on; for login we accept it because the alternative is per-server
	// throttling collapse on shared NAT.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}

// cookieSecure returns true when the cookie should be sent only over
// HTTPS. True if the connection is direct TLS, OR if BehindTLSProxy is
// configured (typical: nginx terminates TLS and proxies http://127.0.0.1).
// (review H4)
func (s *Server) cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if s.auth.BehindTLSProxy {
		// Trust X-Forwarded-Proto only when explicitly told there's a TLS
		// proxy in front; otherwise an attacker could spoof the header.
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
	return false
}

// csrfTokenFor returns the CSRF token for the current session, used by
// templates to embed into hidden form fields.
func (s *Server) csrfTokenFor(r *http.Request) string {
	sid, err := r.Cookie(cookieSession)
	if err != nil || sid == nil {
		return ""
	}
	sess := s.sessions.lookup(sid.Value)
	if sess == nil {
		return ""
	}
	return sess.csrf
}

// ---------------------------------------------------------------------------
// Login / logout handlers
// ---------------------------------------------------------------------------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Enabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderStandalone(w, "login", map[string]any{
			"Title":   "Login",
			"Version": version.Version,
			"Next":    r.URL.Query().Get("next"),
		})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// (review H1) Throttle by client IP — bcrypt is slow but not slow
		// enough for an unbounded attacker. 10 failures / 5 min.
		ip := clientIP(r)
		if !s.sessions.loginAllowed(ip) {
			s.audit(r.Context(), "anonymous", "auth.login_throttled", map[string]any{"ip": ip})
			http.Error(w, "too many failed attempts; try again later", http.StatusTooManyRequests)
			return
		}
		user := strings.TrimSpace(r.FormValue("username"))
		pw := r.FormValue("password")
		if user != s.auth.Username || !verifyPassword(pw, s.auth.PasswordHash) {
			s.sessions.recordLoginFailure(ip)
			s.audit(r.Context(), "anonymous", "auth.login_failed", map[string]any{"username": user, "ip": ip})
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		sid, tok := s.sessions.issue(user, s.auth.SessionTTL)
		secure := s.cookieSecure(r)
		http.SetCookie(w, &http.Cookie{
			Name: cookieSession, Value: sid, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
			MaxAge: int(s.auth.SessionTTL / time.Second),
			Secure: secure,
		})
		http.SetCookie(w, &http.Cookie{
			Name: cookieCSRF, Value: tok, Path: "/",
			SameSite: http.SameSiteStrictMode,
			MaxAge:   int(s.auth.SessionTTL / time.Second),
			Secure:   secure,
		})
		s.audit(r.Context(), user, "auth.login", nil)
		next := r.FormValue("next")
		if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
			next = "/"
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sid, err := r.Cookie(cookieSession); err == nil && sid != nil {
		s.sessions.revoke(sid.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieSession, Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: cookieCSRF, Value: "", Path: "/", MaxAge: -1})
	s.audit(r.Context(), authedUser(r), "auth.logout", nil)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// runSessionGC starts a goroutine that periodically prunes expired sessions.
func (s *Server) runSessionGC(ctx context.Context) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sessions.gcExpired()
		}
	}
}
