package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// apiPrincipal is what a successful Bearer-auth attempt puts into context.
// Downstream handlers read it via apiCaller(r) to identify the actor for
// audit logging and scope enforcement.
type apiPrincipal struct {
	TokenID string
	Name    string
	Scope   store.APITokenScope
	Actor   string // "api:<token_name>" — goes into audit_log.actor
}

type apiPrincipalKey struct{}

func apiCaller(r *http.Request) *apiPrincipal {
	if v, ok := r.Context().Value(apiPrincipalKey{}).(*apiPrincipal); ok {
		return v
	}
	return nil
}

// writeAPIError emits a consistent JSON error envelope.
func writeAPIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// extractBearer returns the raw token from an Authorization: Bearer header,
// or "" if the header is missing or malformed.
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// requireAPIAuth wraps a handler with Bearer-token auth and minimum-scope
// enforcement. The scope hierarchy is read < investigate < admin; a token
// satisfies the requirement if its scope level is at least need.
func (s *Server) requireAPIAuth(need store.APITokenScope, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := extractBearer(r)
		if raw == "" || !strings.HasPrefix(raw, store.APITokenPrefix()) {
			writeAPIError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		tok, err := s.store.LookupAPIToken(r.Context(), raw)
		if err != nil {
			if errors.Is(err, store.ErrAPITokenInvalid) {
				writeAPIError(w, http.StatusUnauthorized, "token invalid, revoked, or expired")
				return
			}
			writeAPIError(w, http.StatusInternalServerError, "token lookup failed")
			return
		}
		if !scopeSatisfies(tok.Scope, need) {
			writeAPIError(w, http.StatusForbidden, "token scope insufficient")
			return
		}
		// last_used_at is best-effort and must not block the request.
		go func(id string) {
			_ = s.store.TouchAPIToken(context.Background(), id)
		}(tok.ID)

		p := &apiPrincipal{
			TokenID: tok.ID,
			Name:    tok.Name,
			Scope:   tok.Scope,
			Actor:   "api:" + tok.Name,
		}
		ctx := context.WithValue(r.Context(), apiPrincipalKey{}, p)
		h(w, r.WithContext(ctx))
	}
}

// scopeSatisfies returns true iff have >= need in the scope hierarchy.
func scopeSatisfies(have, need store.APITokenScope) bool {
	return scopeRank(have) >= scopeRank(need)
}

func scopeRank(s store.APITokenScope) int {
	switch s {
	case store.APIScopeRead:
		return 1
	case store.APIScopeInvestigate:
		return 2
	case store.APIScopeAdmin:
		return 3
	}
	return 0
}
