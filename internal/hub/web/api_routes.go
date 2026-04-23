package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// registerAPIRoutes wires /api/v1/* handlers onto the provided mux. Split
// from Routes() so the mux + middleware composition is easier to follow —
// every API path is wrapped by requireAPIAuth with the minimum scope it
// needs, and never by the cookie/CSRF requireAuth chain.
func (s *Server) registerAPIRoutes(mux *http.ServeMux) {
	// Inventory (read scope).
	mux.HandleFunc("/api/v1/hosts", s.requireAPIAuth(store.APIScopeRead, s.apiListHosts))
	mux.HandleFunc("/api/v1/hosts/", s.requireAPIAuth(store.APIScopeRead, s.apiGetHost))

	// Investigations list / create.
	mux.HandleFunc("/api/v1/investigations", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.requireAPIAuth(store.APIScopeRead, s.apiListInvestigations)(w, r)
		case http.MethodPost:
			s.requireAPIAuth(store.APIScopeInvestigate, s.apiStartInvestigation)(w, r)
		default:
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})

	// /api/v1/investigations/{id}/... — sub-routes dispatched by the last
	// path segment. Keeping a single handler keeps the mux flat and avoids
	// a router dependency.
	mux.HandleFunc("/api/v1/investigations/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/investigations/")
		parts := strings.Split(rest, "/")
		if len(parts) == 0 || parts[0] == "" {
			writeAPIError(w, http.StatusNotFound, "not found")
			return
		}
		invID := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
		}
		switch sub {
		case "":
			if r.Method != http.MethodGet {
				writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			s.requireAPIAuth(store.APIScopeRead, func(w http.ResponseWriter, r *http.Request) {
				s.apiGetInvestigation(w, r, invID)
			})(w, r)
		case "messages":
			s.requireAPIAuth(store.APIScopeRead, func(w http.ResponseWriter, r *http.Request) {
				s.apiListMessages(w, r, invID)
			})(w, r)
		case "tool_calls":
			s.requireAPIAuth(store.APIScopeRead, func(w http.ResponseWriter, r *http.Request) {
				s.apiListToolCalls(w, r, invID)
			})(w, r)
		case "findings":
			s.requireAPIAuth(store.APIScopeRead, func(w http.ResponseWriter, r *http.Request) {
				s.apiListFindings(w, r, invID)
			})(w, r)
		case "decide":
			s.requireAPIAuth(store.APIScopeInvestigate, func(w http.ResponseWriter, r *http.Request) {
				s.apiDecide(w, r, invID)
			})(w, r)
		case "extend":
			s.requireAPIAuth(store.APIScopeInvestigate, func(w http.ResponseWriter, r *http.Request) {
				s.apiExtend(w, r, invID)
			})(w, r)
		case "finalize":
			s.requireAPIAuth(store.APIScopeInvestigate, func(w http.ResponseWriter, r *http.Request) {
				s.apiFinalize(w, r, invID)
			})(w, r)
		case "hypothesis":
			s.requireAPIAuth(store.APIScopeInvestigate, func(w http.ResponseWriter, r *http.Request) {
				s.apiHypothesis(w, r, invID)
			})(w, r)
		case "auto-approve":
			s.requireAPIAuth(store.APIScopeInvestigate, func(w http.ResponseWriter, r *http.Request) {
				s.apiAutoApprove(w, r, invID)
			})(w, r)
		case "events":
			s.requireAPIAuth(store.APIScopeRead, func(w http.ResponseWriter, r *http.Request) {
				s.apiStreamEvents(w, r, invID)
			})(w, r)
		default:
			writeAPIError(w, http.StatusNotFound, "not found")
		}
	})

	// Finding actions.
	mux.HandleFunc("/api/v1/findings/", func(w http.ResponseWriter, r *http.Request) {
		s.requireAPIAuth(store.APIScopeInvestigate, s.apiFindingAction)(w, r)
	})
}

// writeJSON is the canonical JSON emitter: sets Content-Type, pretty-prints
// for human curl inspection, and swallows the encoder's write error (the
// connection is already compromised at that point anyway).
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// readJSONBody parses the request body into dst, capped at 1 MiB. Emits a
// 400 on any error and returns false so the handler can bail.
func readJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return false
	}
	return true
}

func auditAPI(s *Server, r *http.Request, action string, details map[string]any) {
	p := apiCaller(r)
	actor := "api"
	if p != nil {
		actor = p.Actor
	}
	s.audit(r.Context(), actor, action, details)
}
