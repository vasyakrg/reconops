package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// investigationView flattens a store.Investigation into wire-friendly fields
// and replaces sql.NullString with plain strings (null → "").
type investigationView struct {
	ID                    string   `json:"id"`
	Goal                  string   `json:"goal"`
	Status                string   `json:"status"`
	CreatedBy             string   `json:"created_by"`
	CreatedAt             string   `json:"created_at"`
	UpdatedAt             string   `json:"updated_at"`
	Model                 string   `json:"model"`
	TotalPromptTokens     int      `json:"total_prompt_tokens"`
	TotalCompletionTokens int      `json:"total_completion_tokens"`
	TotalToolCalls        int      `json:"total_tool_calls"`
	CompactionTokens      int      `json:"compaction_tokens"`
	ExtraSteps            int      `json:"extra_steps"`
	ExtraTokens           int      `json:"extra_tokens"`
	AutoApprove           bool     `json:"auto_approve"`
	AllowedHosts          []string `json:"allowed_hosts"`
	SummaryJSON           string   `json:"summary_json,omitempty"`
	MaxSteps              int      `json:"max_steps"`
	MaxTokens             int      `json:"max_tokens"`
}

func investigationToView(inv store.Investigation, maxSteps, maxTokens int) investigationView {
	v := investigationView{
		ID:                    inv.ID,
		Goal:                  inv.Goal,
		Status:                inv.Status,
		CreatedBy:             inv.CreatedBy,
		CreatedAt:             inv.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:             inv.UpdatedAt.UTC().Format(time.RFC3339),
		Model:                 inv.Model,
		TotalPromptTokens:     inv.TotalPromptTokens,
		TotalCompletionTokens: inv.TotalCompletionTokens,
		TotalToolCalls:        inv.TotalToolCalls,
		CompactionTokens:      inv.CompactionTokens,
		ExtraSteps:            inv.ExtraSteps,
		ExtraTokens:           inv.ExtraTokens,
		AutoApprove:           inv.AutoApprove,
		AllowedHosts:          inv.AllowedHosts,
		MaxSteps:              maxSteps,
		MaxTokens:             maxTokens,
	}
	if inv.SummaryJSON.Valid {
		v.SummaryJSON = inv.SummaryJSON.String
	}
	return v
}

// requireLoop returns the attached investigator.Loop or writes 503 and
// returns nil if the LLM is not configured. Centralises the nil-check so
// every handler that needs the loop can guard cheaply.
func (s *Server) requireLoop(w http.ResponseWriter) bool {
	if s.loop == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "investigator disabled (RECON_LLM_API_KEY not set)")
		return false
	}
	return true
}

func (s *Server) apiListInvestigations(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	status := r.URL.Query().Get("status")
	invs, err := s.store.ListInvestigations(r.Context(), limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list: "+err.Error())
		return
	}
	maxSteps, maxTokens := 0, 0
	if s.loop != nil {
		maxSteps, maxTokens = s.loop.Budgets()
	}
	out := make([]investigationView, 0, len(invs))
	for _, inv := range invs {
		if status != "" && inv.Status != status {
			continue
		}
		out = append(out, investigationToView(inv, maxSteps, maxTokens))
	}
	writeJSON(w, http.StatusOK, map[string]any{"investigations": out})
}

type startInvestigationReq struct {
	Goal         string   `json:"goal"`
	AllowedHosts []string `json:"allowed_hosts"`
	AutoApprove  bool     `json:"auto_approve"`
}

func (s *Server) apiStartInvestigation(w http.ResponseWriter, r *http.Request) {
	if !s.requireLoop(w) {
		return
	}
	var req startInvestigationReq
	if !readJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Goal) == "" {
		writeAPIError(w, http.StatusBadRequest, "goal required")
		return
	}
	actor := "api"
	if p := apiCaller(r); p != nil {
		actor = p.Actor
	}
	id, err := s.loop.Start(r.Context(), req.Goal, actor, req.AllowedHosts...)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.AutoApprove {
		_ = s.store.SetAutoApprove(r.Context(), id, true)
	}
	auditAPI(s, r, "investigation.start",
		map[string]any{"investigation_id": id, "goal": req.Goal, "via": "api"})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": "active"})
}

func (s *Server) apiGetInvestigation(w http.ResponseWriter, r *http.Request, id string) {
	inv, err := s.store.GetInvestigation(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "investigation not found")
		return
	}
	maxSteps, maxTokens := 0, 0
	if s.loop != nil {
		maxSteps, maxTokens = s.loop.Budgets()
	}
	writeJSON(w, http.StatusOK, investigationToView(inv, maxSteps, maxTokens))
}

func (s *Server) apiListMessages(w http.ResponseWriter, r *http.Request, invID string) {
	includeArchived := r.URL.Query().Get("include_archived") == "1"
	afterSeq := 0
	if v := r.URL.Query().Get("after_seq"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			afterSeq = n
		}
	}
	msgs, err := s.store.ListMessages(r.Context(), invID, includeArchived)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type msgView struct {
		Seq           int    `json:"seq"`
		Role          string `json:"role"`
		Content       string `json:"content"`
		ToolCallID    string `json:"tool_call_id,omitempty"`
		ToolCallsJSON string `json:"tool_calls_json,omitempty"`
		Timestamp     string `json:"timestamp"`
		Archived      bool   `json:"archived,omitempty"`
	}
	out := make([]msgView, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq <= afterSeq {
			continue
		}
		mv := msgView{
			Seq:       m.Seq,
			Role:      m.Role,
			Content:   m.Content,
			Timestamp: m.Timestamp.UTC().Format(time.RFC3339),
			Archived:  m.Archived,
		}
		if m.ToolCallID.Valid {
			mv.ToolCallID = m.ToolCallID.String
		}
		if m.ToolCallsJSON.Valid {
			mv.ToolCallsJSON = m.ToolCallsJSON.String
		}
		out = append(out, mv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

func (s *Server) apiListToolCalls(w http.ResponseWriter, r *http.Request, invID string) {
	tcs, err := s.store.ListToolCalls(r.Context(), invID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	statusFilter := r.URL.Query().Get("status")
	type tcView struct {
		ID         string `json:"id"`
		Seq        int    `json:"seq"`
		Tool       string `json:"tool"`
		Status     string `json:"status"`
		Rationale  string `json:"rationale"`
		InputJSON  string `json:"input_json"`
		ResultJSON string `json:"result_json,omitempty"`
		TaskID     string `json:"task_id,omitempty"`
		DecidedBy  string `json:"decided_by,omitempty"`
		CreatedAt  string `json:"created_at"`
	}
	out := make([]tcView, 0, len(tcs))
	for _, tc := range tcs {
		if statusFilter != "" && tc.Status != statusFilter {
			continue
		}
		v := tcView{
			ID: tc.ID, Seq: tc.Seq, Tool: tc.Tool, Status: tc.Status,
			Rationale: tc.Rationale, InputJSON: tc.InputJSON,
			CreatedAt: tc.CreatedAt.UTC().Format(time.RFC3339),
		}
		if tc.ResultJSON.Valid {
			v.ResultJSON = tc.ResultJSON.String
		}
		if tc.TaskID.Valid {
			v.TaskID = tc.TaskID.String
		}
		if tc.DecidedBy.Valid {
			v.DecidedBy = tc.DecidedBy.String
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tool_calls": out})
}

func (s *Server) apiListFindings(w http.ResponseWriter, r *http.Request, invID string) {
	findings, err := s.store.ListFindings(r.Context(), invID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": findings})
}

type decideReq struct {
	Decision     string `json:"decision"` // approve | skip | edit | end
	NewInputJSON string `json:"new_input_json,omitempty"`
}

func (s *Server) apiDecide(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLoop(w) {
		return
	}
	var req decideReq
	if !readJSONBody(w, r, &req) {
		return
	}
	actor := "api"
	if p := apiCaller(r); p != nil {
		actor = p.Actor
	}
	var err error
	if req.Decision == "edit" {
		err = s.loop.DecideWithEdit(r.Context(), invID, req.Decision, req.NewInputJSON, actor)
	} else {
		err = s.loop.Decide(r.Context(), invID, req.Decision, actor)
	}
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditAPI(s, r, "investigation.decide",
		map[string]any{"investigation_id": invID, "decision": req.Decision})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type extendReq struct {
	ExtraSteps  int `json:"extra_steps"`
	ExtraTokens int `json:"extra_tokens"`
}

func (s *Server) apiExtend(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLoop(w) {
		return
	}
	var req extendReq
	if !readJSONBody(w, r, &req) {
		return
	}
	actor := "api"
	if p := apiCaller(r); p != nil {
		actor = p.Actor
	}
	if err := s.loop.Extend(r.Context(), invID, req.ExtraSteps, req.ExtraTokens, actor); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditAPI(s, r, "investigation.extend",
		map[string]any{"investigation_id": invID, "extra_steps": req.ExtraSteps, "extra_tokens": req.ExtraTokens})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiFinalize(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLoop(w) {
		return
	}
	actor := "api"
	if p := apiCaller(r); p != nil {
		actor = p.Actor
	}
	if err := s.loop.Finalize(r.Context(), invID, actor); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditAPI(s, r, "investigation.finalize", map[string]any{"investigation_id": invID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type hypothesisReq struct {
	Claim       string `json:"claim"`
	Expected    string `json:"expected"`
	Instruction string `json:"instruction"`
}

func (s *Server) apiHypothesis(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLoop(w) {
		return
	}
	var req hypothesisReq
	if !readJSONBody(w, r, &req) {
		return
	}
	actor := "api"
	if p := apiCaller(r); p != nil {
		actor = p.Actor
	}
	if err := s.loop.InjectHypothesis(r.Context(), invID, req.Claim, req.Expected, req.Instruction, actor); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditAPI(s, r, "investigation.hypothesis",
		map[string]any{"investigation_id": invID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type autoApproveReq struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) apiAutoApprove(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req autoApproveReq
	if !readJSONBody(w, r, &req) {
		return
	}
	if err := s.store.SetAutoApprove(r.Context(), invID, req.Enabled); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	auditAPI(s, r, "investigation.auto_approve",
		map[string]any{"investigation_id": invID, "enabled": req.Enabled})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// apiFindingAction handles POST /api/v1/findings/{id}/{action}. Actions:
// pin, unpin, ignore, unignore. Same semantics as the cookie-auth form.
func (s *Server) apiFindingAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/findings/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeAPIError(w, http.StatusBadRequest, "path must be /api/v1/findings/{id}/{action}")
		return
	}
	id, action := parts[0], parts[1]
	var err error
	switch action {
	case "pin":
		err = s.store.SetFindingPinned(r.Context(), id, true)
	case "unpin":
		err = s.store.SetFindingPinned(r.Context(), id, false)
	case "ignore":
		err = s.store.SetFindingIgnored(r.Context(), id, true)
	case "unignore":
		err = s.store.SetFindingIgnored(r.Context(), id, false)
	default:
		writeAPIError(w, http.StatusBadRequest, "action must be pin|unpin|ignore|unignore")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	auditAPI(s, r, "finding."+action, map[string]any{"finding_id": id})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
