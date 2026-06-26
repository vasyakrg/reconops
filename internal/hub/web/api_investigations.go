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
	CachedTokens          int      `json:"cached_tokens"`
	TokenCalibrationRatio float64  `json:"token_calibration_ratio,omitempty"`
	ExtraSteps            int      `json:"extra_steps"`
	ExtraTokens           int      `json:"extra_tokens"`
	AutoApprove           bool     `json:"auto_approve"`
	AutoRunUntilSteps     int      `json:"auto_run_until_steps"`
	AutoRunUntilTokens    int      `json:"auto_run_until_tokens"`
	ModelProfile          string   `json:"model_profile,omitempty"`
	AllowedHosts          []string `json:"allowed_hosts"`
	SummaryJSON           string   `json:"summary_json,omitempty"`
	TerminalKind          string   `json:"terminal_kind,omitempty"`
	TerminalReason        string   `json:"terminal_reason,omitempty"`
	TerminalRecoverable   bool     `json:"terminal_recoverable"`
	TerminalSource        string   `json:"terminal_source,omitempty"`
	TerminalDetail        string   `json:"terminal_detail,omitempty"`
	MaxSteps              int      `json:"max_steps"`
	MaxTokens             int      `json:"max_tokens"`
	NotebookPath          string   `json:"notebook_path,omitempty"`
	MemoryCount           int      `json:"memory_count"`
}

func investigationToView(inv store.Investigation, maxSteps, maxTokens int, terminal terminalPayloadView) investigationView {
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
		CachedTokens:          inv.TotalCachedTokens,
		TokenCalibrationRatio: inv.TokenCalibrationRatio,
		ExtraSteps:            inv.ExtraSteps,
		ExtraTokens:           inv.ExtraTokens,
		AutoApprove:           inv.AutoApprove,
		AutoRunUntilSteps:     inv.AutoRunUntilSteps,
		AutoRunUntilTokens:    inv.AutoRunUntilTokens,
		ModelProfile:          inv.ModelProfile,
		AllowedHosts:          inv.AllowedHosts,
		MaxSteps:              maxSteps,
		MaxTokens:             maxTokens,
	}
	if inv.SummaryJSON.Valid {
		v.SummaryJSON = inv.SummaryJSON.String
	}
	if terminal.Present {
		v.TerminalKind = terminal.Kind
		v.TerminalReason = terminal.Reason
		v.TerminalRecoverable = terminal.Recoverable
		v.TerminalSource = terminal.Source
		v.TerminalDetail = terminal.Detail
	}
	return v
}

// requireLoop returns the attached investigator.Loop or writes 503 and
// returns nil if the LLM is not configured. Centralises the nil-check so
// every handler that needs the loop can guard cheaply.
func (s *Server) requireLoop(w http.ResponseWriter) bool {
	if !s.availability.Enabled {
		writeAPIError(w, http.StatusServiceUnavailable, "investigator disabled: "+s.availability.ConfigHint)
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
		out = append(out, investigationToView(inv, maxSteps, maxTokens, s.terminalPayloadView(inv.ID, inv.SummaryJSON)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"investigations": out})
}

type startInvestigationReq struct {
	Goal         string   `json:"goal"`
	AllowedHosts []string `json:"allowed_hosts"`
	AutoApprove  bool     `json:"auto_approve"`
	ModelProfile string   `json:"model_profile"`
	// PriorIDs: optional operator-selected prior investigations to attach,
	// merged with the automatic host-scoped selection.
	PriorIDs []string `json:"prior_ids,omitempty"`
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
	profile := strings.TrimSpace(req.ModelProfile)
	if profile != "" && !s.knownLLMProfile(profile) {
		writeAPIError(w, http.StatusBadRequest, "unknown model profile")
		return
	}
	id, err := s.loop.StartWithPriors(r.Context(), req.Goal, actor, profile, req.PriorIDs, req.AllowedHosts)
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
	v := investigationToView(inv, maxSteps, maxTokens, s.terminalPayloadView(inv.ID, inv.SummaryJSON))
	if s.nb != nil {
		if _, ok := s.nb.Path(id); ok {
			v.NotebookPath = "/investigations/notebook/" + id
		}
	}
	if mems, merr := s.store.ListMemory(r.Context(), id, 1000); merr == nil {
		v.MemoryCount = len(mems)
	}
	writeJSON(w, http.StatusOK, v)
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

// apiListToolCalls / apiListFindings return the store's native ascending order
// (oldest-first). Only the live web detail page reverses to newest-first (see
// investigationDetailData); programmatic consumers get a stable, paginatable
// chronological order so they can sort client-side as they wish.
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

type continueReq struct {
	Message string `json:"message"`
}

func (s *Server) apiContinueInvestigation(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLoop(w) {
		s.log.Info("api continue investigation rejected",
			"investigation_id", invID, "failure_class", "disabled", "reason_class", s.availability.DisabledReason)
		return
	}
	var req continueReq
	if !readJSONBody(w, r, &req) {
		return
	}
	actor := "api"
	if p := apiCaller(r); p != nil {
		actor = p.Actor
	}
	// Pre-check status so we never report success for a non-reopenable
	// investigation. ResumeAborted no-ops (returns nil) when its claim does not
	// win the transition, which would otherwise let active/waiting/paused
	// return {ok:true,status:"active"} while doing nothing.
	inv, err := s.store.GetInvestigation(r.Context(), invID)
	if err != nil {
		s.log.Warn("api continue investigation lookup failed", "investigation_id", invID, "err", err)
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		s.log.Info("api continue investigation rejected",
			"investigation_id", invID, "status", inv.Status, "failure_class", "empty_message")
		writeAPIError(w, http.StatusBadRequest, "message required")
		return
	}
	if inv.Status != "aborted" && inv.Status != "done" {
		s.log.Info("api continue investigation rejected",
			"investigation_id", invID, "status", inv.Status, "failure_class", "wrong_status")
		writeAPIError(w, http.StatusBadRequest, "only aborted or completed investigations can be continued; current status: "+inv.Status)
		return
	}
	if err := s.loop.ResumeAborted(r.Context(), invID, req.Message, actor); err != nil {
		s.log.Info("api continue investigation rejected",
			"investigation_id", invID,
			"status", inv.Status,
			"failure_class", "resume_error",
			"err", sanitizeOperatorError(err),
			"message_chars", len(req.Message))
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditAPI(s, r, "investigation.resume_aborted",
		map[string]any{"investigation_id": invID, "message_chars": len(req.Message), "prior_status": inv.Status})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "active"})
}

// apiRetryInvestigation re-sends the same last request for a transient LLM
// abort — no message body required. Mirror of the browser /investigations/retry
// handler.
func (s *Server) apiRetryInvestigation(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLoop(w) {
		s.log.Info("api retry investigation rejected",
			"investigation_id", invID, "failure_class", "disabled", "reason_class", s.availability.DisabledReason)
		return
	}
	actor := "api"
	if p := apiCaller(r); p != nil {
		actor = p.Actor
	}
	if err := s.loop.RetryLastStep(r.Context(), invID, actor); err != nil {
		failureClass := "retry_error"
		status := ""
		if inv, getErr := s.store.GetInvestigation(r.Context(), invID); getErr == nil {
			status = inv.Status
			if inv.Status != "aborted" {
				failureClass = "wrong_status"
			}
		}
		s.log.Info("api retry investigation rejected",
			"investigation_id", invID, "status", status, "failure_class", failureClass,
			"err", sanitizeOperatorError(err))
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditAPI(s, r, "investigation.retry_transient", map[string]any{"investigation_id": invID})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "active"})
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

type autonomousReq struct {
	// Disarm clears an armed burst (operator takes over). When false (default),
	// the request ARMS a burst bounded by Steps / Tokens deltas from now.
	Disarm bool `json:"disarm"`
	Steps  int  `json:"steps"`
	Tokens int  `json:"tokens"`
}

func (s *Server) apiAutonomous(w http.ResponseWriter, r *http.Request, invID string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLoop(w) {
		return
	}
	var req autonomousReq
	if !readJSONBody(w, r, &req) {
		return
	}
	actor := "api"
	if p := apiCaller(r); p != nil {
		actor = p.Actor
	}
	if req.Disarm {
		if err := s.loop.DisarmAutonomousRun(r.Context(), invID, actor); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		auditAPI(s, r, "investigation.autonomous_disarm", map[string]any{"investigation_id": invID})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err := s.loop.StartAutonomousRun(r.Context(), invID, req.Steps, req.Tokens, actor); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditAPI(s, r, "investigation.autonomous_run",
		map[string]any{"investigation_id": invID, "steps": req.Steps, "tokens": req.Tokens})
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
