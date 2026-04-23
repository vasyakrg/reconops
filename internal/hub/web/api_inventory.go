package web

import (
	"net/http"
	"strings"
)

// hostView is the trimmed-down host representation for the API. We drop
// the cert fingerprint — it is operator-only material.
type hostView struct {
	ID           string            `json:"id"`
	AgentVersion string            `json:"agent_version"`
	Labels       map[string]string `json:"labels"`
	Facts        map[string]string `json:"facts"`
	FirstSeenAt  string            `json:"first_seen_at"`
	LastSeenAt   string            `json:"last_seen_at"`
	Status       string            `json:"status"`
}

func (s *Server) apiListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list hosts: "+err.Error())
		return
	}
	out := make([]hostView, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, hostView{
			ID:           h.ID,
			AgentVersion: h.AgentVersion,
			Labels:       h.Labels,
			Facts:        h.Facts,
			FirstSeenAt:  h.FirstSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
			LastSeenAt:   h.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
			Status:       h.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

func (s *Server) apiGetHost(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/hosts/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "host id required")
		return
	}
	h, err := s.store.GetHost(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "host not found")
		return
	}
	writeJSON(w, http.StatusOK, hostView{
		ID:           h.ID,
		AgentVersion: h.AgentVersion,
		Labels:       h.Labels,
		Facts:        h.Facts,
		FirstSeenAt:  h.FirstSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		LastSeenAt:   h.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		Status:       h.Status,
	})
}
