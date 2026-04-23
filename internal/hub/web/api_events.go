package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/vasyakrg/recon/internal/hub/investigator"
)

// apiStreamEvents is SSE with JSON payloads for remote investigator clients.
// Flow:
//  1. Verify the investigation exists (404 otherwise).
//  2. Emit a `snapshot` event with current status + counts so the client can
//     resync without a separate /investigations/{id} GET.
//  3. Subscribe to the per-investigation Bus and forward each event.
//  4. Send a `heartbeat` comment every 25s so proxies don't idle out.
//
// The handler returns when the client disconnects (r.Context() done) or the
// hub shuts down. The Bus subscription is torn down either way.
func (s *Server) apiStreamEvents(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.loop == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "investigator disabled")
		return
	}
	inv, err := s.store.GetInvestigation(r.Context(), invID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "investigation not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx: disable proxy buffering.
	w.WriteHeader(http.StatusOK)

	// Snapshot event — gives the client enough state to recover from a
	// reconnect without needing to re-query list endpoints.
	maxSteps, maxTokens := s.loop.Budgets()
	snapshot := map[string]any{
		"investigation": investigationToView(inv, maxSteps, maxTokens),
		"server_time":   time.Now().UTC().Format(time.RFC3339),
	}
	writeSSEEvent(w, "snapshot", snapshot)
	flusher.Flush()

	ch, unsubscribe := s.loop.Bus().Subscribe(invID)
	defer unsubscribe()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			writeSSEEventRaw(w, string(ev.Type), ev.Data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat %d\n\n", time.Now().Unix())
			flusher.Flush()
		}
	}
}

// writeSSEEvent serialises v and emits it as an SSE `event: name` frame.
func writeSSEEvent(w http.ResponseWriter, name string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"error":"marshal failed"}`)
	}
	writeSSEEventRaw(w, name, body)
}

// writeSSEEventRaw skips the marshal step for payloads that already arrive
// as JSON bytes (Bus events hold json.RawMessage).
func writeSSEEventRaw(w http.ResponseWriter, name string, body []byte) {
	fmt.Fprintf(w, "event: %s\n", name)
	fmt.Fprintf(w, "data: %s\n\n", body)
}

// Ensure we link the investigator package symbol even if api_investigations
// doesn't reference it (investigator.EventType would otherwise produce an
// unused-import error when the file is built in isolation during testing).
var _ investigator.EventType = investigator.EventMessageAppended
