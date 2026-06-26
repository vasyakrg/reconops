package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Investigation struct {
	ID                    string
	Goal                  string
	Status                string // active|waiting|paused|done|aborted
	CreatedBy             string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	Model                 string
	BaseURL               string
	TotalPromptTokens     int
	TotalCompletionTokens int
	TotalToolCalls        int
	CompactionTokens      int // tokens spent on internal compaction calls (review C2)
	TotalCachedTokens     int // prompt tokens served from provider cache (Task 4)
	// TokenCalibrationRatio is the EWMA bytes/token ratio used to estimate the
	// compaction trigger (Task 6). 0 means uncalibrated → default ratio.
	TokenCalibrationRatio float64
	SummaryJSON           sql.NullString
	// AllowedHosts: empty means "all hosts" (legacy behaviour). When set,
	// list_hosts only surfaces these and collect/collect_batch reject any
	// host_id outside the list.
	AllowedHosts []string
	// Budget extensions added by the operator after the global cap from
	// hub.yaml was hit — additive on top of (max_steps, max_tokens).
	ExtraSteps  int
	ExtraTokens int
	// AutoApprove: when true, every operator-gated tool_call (collect,
	// collect_batch, add_finding, search_artifact, get_full_result,
	// compare_across_hosts, mark_done, ask_operator) is auto-approved
	// without operator click. Toggleable per-investigation from the
	// detail page.
	AutoApprove bool
	// AutoRunUntilSteps / AutoRunUntilTokens: absolute totals targets bounding an
	// operator-armed autonomous burst (migration 0020). While armed (either > 0)
	// the loop auto-approves probe tool_calls until the matching running total
	// reaches the target, then pauses for review and disarms. 0 = not armed on
	// that axis. Independent of AutoApprove (the unbounded manual toggle).
	AutoRunUntilSteps  int
	AutoRunUntilTokens int
	// ModelProfile pins every LLM operation in this investigation to a named
	// model profile (Task 14). Empty means "auto" — the router selects a
	// profile per operation by role.
	ModelProfile string
	// Priors: prior done-investigation IDs whose conclusions were attached to
	// this run at start (auto host-overlap selection + operator-selected).
	// Informational — surfaced on the detail page; empty when none.
	Priors []string
}

// PriorInvestigation is the compact slice of a COMPLETED investigation used to
// build the cross-investigation priors digest injected into new investigations.
// SummaryJSON is the raw mark_done post-mortem; parse it with
// ParseInvestigationTerminalPayload (which tolerates legacy / missing payloads).
type PriorInvestigation struct {
	ID           string
	Goal         string
	Status       string
	CreatedAt    time.Time
	AllowedHosts []string
	SummaryJSON  sql.NullString
}

type Message struct {
	ID              int64
	InvestigationID string
	Seq             int
	Role            string
	Content         string
	ToolCallID      sql.NullString
	ToolCallsJSON   sql.NullString // serialized []llm.ToolCall for assistant rows (C1)
	Timestamp       time.Time
	Archived        bool
}

type ToolCallRow struct {
	ID              string
	InvestigationID string
	Seq             int
	Tool            string
	InputJSON       string
	Rationale       string
	Status          string
	DecidedBy       sql.NullString
	TaskID          sql.NullString
	CreatedAt       time.Time
	DecidedAt       sql.NullTime
	ResultJSON      sql.NullString
	BroadConfirmed  bool // operator passed broad-selector gate (week 4 §9)
}

type Finding struct {
	ID              string
	InvestigationID string
	Severity        string
	Code            string
	Message         string
	EvidenceJSON    string
	Pinned          bool
	Ignored         bool
	CreatedAt       time.Time
}

func (s *Store) InsertInvestigation(ctx context.Context, inv Investigation) error {
	now := time.Now().UTC()
	var allowed sql.NullString
	if len(inv.AllowedHosts) > 0 {
		b, err := json.Marshal(inv.AllowedHosts)
		if err != nil {
			return err
		}
		allowed = sql.NullString{String: string(b), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO investigations
          (id, goal, status, created_by, created_at, updated_at, model, base_url, allowed_hosts_json, model_profile)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.Goal, inv.Status, inv.CreatedBy, now, now, inv.Model, inv.BaseURL, allowed, inv.ModelProfile)
	return err
}

func (s *Store) GetInvestigation(ctx context.Context, id string) (Investigation, error) {
	var inv Investigation
	var allowed sql.NullString
	var priors sql.NullString
	var auto int
	err := s.db.QueryRowContext(ctx, `
        SELECT id, goal, status, created_by, created_at, updated_at, model, base_url,
               total_prompt_tokens, total_completion_tokens, total_tool_calls, compaction_tokens, total_cached_tokens, token_calibration_ratio, summary_json,
               allowed_hosts_json, extra_steps, extra_tokens, auto_approve, model_profile, priors_json,
               auto_run_until_steps, auto_run_until_tokens
          FROM investigations WHERE id=?`, id).
		Scan(&inv.ID, &inv.Goal, &inv.Status, &inv.CreatedBy, &inv.CreatedAt, &inv.UpdatedAt,
			&inv.Model, &inv.BaseURL,
			&inv.TotalPromptTokens, &inv.TotalCompletionTokens, &inv.TotalToolCalls, &inv.CompactionTokens, &inv.TotalCachedTokens, &inv.TokenCalibrationRatio, &inv.SummaryJSON,
			&allowed, &inv.ExtraSteps, &inv.ExtraTokens, &auto, &inv.ModelProfile, &priors,
			&inv.AutoRunUntilSteps, &inv.AutoRunUntilTokens)
	inv.AutoApprove = auto == 1
	if errors.Is(err, sql.ErrNoRows) {
		return Investigation{}, fmt.Errorf("investigation %s not found", id)
	}
	if allowed.Valid && allowed.String != "" {
		_ = json.Unmarshal([]byte(allowed.String), &inv.AllowedHosts)
	}
	if priors.Valid && priors.String != "" {
		_ = json.Unmarshal([]byte(priors.String), &inv.Priors)
	}
	return inv, err
}

// SetInvestigationPriors records which prior investigations were attached to a
// run (auto host-overlap + operator-selected) for operator visibility on the
// detail page. Stored as a JSON id array; an empty slice clears it.
func (s *Store) SetInvestigationPriors(ctx context.Context, id string, ids []string) error {
	val := ""
	if len(ids) > 0 {
		b, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		val = string(b)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET priors_json=?, updated_at=? WHERE id=?`,
		val, time.Now().UTC(), id)
	return err
}

// scanPriorInvestigations maps prior-investigation rows. Callers MUST select
// (id, goal, status, created_at, allowed_hosts_json, summary_json) in that order.
func scanPriorInvestigations(rows *sql.Rows) ([]PriorInvestigation, error) {
	var out []PriorInvestigation
	for rows.Next() {
		var p PriorInvestigation
		var allowed sql.NullString
		if err := rows.Scan(&p.ID, &p.Goal, &p.Status, &p.CreatedAt, &allowed, &p.SummaryJSON); err != nil {
			return nil, err
		}
		if allowed.Valid && allowed.String != "" {
			_ = json.Unmarshal([]byte(allowed.String), &p.AllowedHosts)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListInvestigationsByIDs returns the investigations whose ids are in the set,
// of ANY status — for rendering attached priors on the detail page and for
// resolving operator-selected priors at Start (which may include aborted/active
// runs, attached for their findings). Non-existent ids are silently skipped.
// Order is created_at DESC.
func (s *Store) ListInvestigationsByIDs(ctx context.Context, ids []string) ([]PriorInvestigation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT id, goal, status, created_at, allowed_hosts_json, summary_json
            FROM investigations
           WHERE id IN (` + strings.Join(placeholders, ",") + `)
           ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPriorInvestigations(rows)
}

// SetAutoApprove flips the per-investigation auto-approve toggle.
func (s *Store) SetAutoApprove(ctx context.Context, id string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET auto_approve = ?, updated_at = ? WHERE id = ?`,
		v, time.Now().UTC(), id)
	return err
}

// SetAutonomousRun arms an autonomous burst: it records the ABSOLUTE totals
// targets (computed by the caller as current totals + operator delta) and flips
// status back to 'active' so a paused investigation resumes (mirrors
// ExtendBudget). At least one target must be > 0 to be "armed". Independent of
// auto_approve — the bounded burst is tracked solely by these targets, so it
// never silently leaves the unbounded toggle on after the burst ends.
func (s *Store) SetAutonomousRun(ctx context.Context, id string, untilSteps, untilTokens int) error {
	if untilSteps < 0 {
		untilSteps = 0
	}
	if untilTokens < 0 {
		untilTokens = 0
	}
	_, err := s.db.ExecContext(ctx, `
        UPDATE investigations
           SET auto_run_until_steps  = ?,
               auto_run_until_tokens = ?,
               status                = 'active',
               updated_at            = ?
         WHERE id = ?`,
		untilSteps, untilTokens, time.Now().UTC(), id)
	return err
}

// DisarmAutonomous clears the autonomous-run targets (0/0 = not armed). Called
// when the burst is consumed (loop pauses for review) or the operator takes
// over. Leaves auto_approve and status untouched — the caller owns those.
func (s *Store) DisarmAutonomous(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET auto_run_until_steps = 0, auto_run_until_tokens = 0, updated_at = ? WHERE id = ?`,
		time.Now().UTC(), id)
	return err
}

// SetModelProfile pins (or clears, with "") the per-investigation model
// routing override (Task 14).
func (s *Store) SetModelProfile(ctx context.Context, id, profile string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET model_profile = ?, updated_at = ? WHERE id = ?`,
		profile, time.Now().UTC(), id)
	return err
}

// ExtendBudget bumps the per-investigation extras and flips status back to
// "active" so the loop resumes. Caller spawns the loop goroutine.
func (s *Store) ExtendBudget(ctx context.Context, id string, extraSteps, extraTokens int) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE investigations
           SET extra_steps  = extra_steps  + ?,
               extra_tokens = extra_tokens + ?,
               status       = 'active',
               updated_at   = ?
         WHERE id = ?`,
		extraSteps, extraTokens, time.Now().UTC(), id)
	return err
}

func (s *Store) UpdateInvestigationStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET status=?, updated_at=? WHERE id=?`,
		status, time.Now().UTC(), id)
	return err
}

// ReactivateInvestigation moves a previously aborted investigation back to
// active and clears the stale terminal summary/error payload. The message
// history and tool-call timeline stay intact so the next LLM turn continues
// from the original context.
func (s *Store) ReactivateInvestigation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET status='active', summary_json=NULL, updated_at=? WHERE id=? AND status='aborted'`,
		time.Now().UTC(), id)
	return err
}

// ClaimAbortedForResume atomically flips an aborted investigation to active
// (clearing the terminal payload) and reports whether THIS call won the
// transition. The single conditional UPDATE is the concurrency gate: a
// double-submitted resume finds the row already active (0 rows affected) and
// gets false, so the caller can no-op instead of appending a second RESUME
// message and double-auditing.
func (s *Store) ClaimAbortedForResume(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET status='active', summary_json=NULL, updated_at=? WHERE id=? AND status='aborted'`,
		time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClaimReopenableForResume atomically flips a REOPENABLE terminal investigation
// (aborted OR done) back to active, clearing the stale terminal payload, and
// reports whether THIS call won the transition. Same single-conditional-UPDATE
// concurrency gate as ClaimAbortedForResume: a double-submitted reopen finds the
// row already active (0 rows affected) and gets false, so the caller no-ops
// instead of appending a second OPERATOR RESUME message and double-auditing.
//
// 'done' is reopenable by operator request — continuing a completed
// investigation in place (done -> active + OPERATOR RESUME) is a deliberate
// reversal of the earlier "done is hard-terminal" choice. The retry path stays
// aborted-only (ClaimAbortedForResume) because re-running the last step of a
// transient abort has no meaning for a cleanly completed run.
func (s *Store) ClaimReopenableForResume(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET status='active', summary_json=NULL, updated_at=? WHERE id=? AND status IN ('aborted','done')`,
		time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AccumulateCompactionTokens tallies prompt+completion tokens spent on
// internal compaction calls. Kept separate from total_*_tokens so the
// investigation budget gate can subtract them (review C2).
func (s *Store) AccumulateCompactionTokens(ctx context.Context, id string, total int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET compaction_tokens = compaction_tokens + ?, updated_at=? WHERE id=?`,
		total, time.Now().UTC(), id)
	return err
}

func (s *Store) AccumulateTokens(ctx context.Context, id string, prompt, completion int) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE investigations
           SET total_prompt_tokens = total_prompt_tokens + ?,
               total_completion_tokens = total_completion_tokens + ?,
               updated_at = ?
         WHERE id = ?`,
		prompt, completion, time.Now().UTC(), id)
	return err
}

// AccumulateCachedTokens tallies prompt tokens that the provider served from
// cache (Task 4). Read-only accounting surfaced in diagnostics; it never feeds
// the budget gate (cached tokens are still real prompt tokens for budgeting).
func (s *Store) AccumulateCachedTokens(ctx context.Context, id string, cached int) error {
	if cached <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET total_cached_tokens = total_cached_tokens + ?, updated_at=? WHERE id=?`,
		cached, time.Now().UTC(), id)
	return err
}

// SetTokenCalibration persists the latest EWMA bytes/token ratio for an
// investigation (Task 6). It is read back on the next turn to estimate the
// compaction trigger. Ignores non-positive ratios (nothing to calibrate yet).
func (s *Store) SetTokenCalibration(ctx context.Context, id string, ratio float64) error {
	if ratio <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET token_calibration_ratio = ?, updated_at=? WHERE id=?`,
		ratio, time.Now().UTC(), id)
	return err
}

func (s *Store) IncrementToolCalls(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET total_tool_calls = total_tool_calls + 1, updated_at=? WHERE id=?`,
		time.Now().UTC(), id)
	return err
}

func (s *Store) FinishInvestigation(ctx context.Context, id, status, summaryJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET status=?, summary_json=?, updated_at=? WHERE id=?`,
		status, summaryJSON, time.Now().UTC(), id)
	return err
}

func (s *Store) ListInvestigations(ctx context.Context, limit int) ([]Investigation, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, goal, status, created_by, created_at, updated_at, model, base_url,
               total_prompt_tokens, total_completion_tokens, total_tool_calls, compaction_tokens, total_cached_tokens, token_calibration_ratio, summary_json
          FROM investigations ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Investigation
	for rows.Next() {
		var inv Investigation
		if err := rows.Scan(&inv.ID, &inv.Goal, &inv.Status, &inv.CreatedBy, &inv.CreatedAt, &inv.UpdatedAt,
			&inv.Model, &inv.BaseURL,
			&inv.TotalPromptTokens, &inv.TotalCompletionTokens, &inv.TotalToolCalls, &inv.CompactionTokens, &inv.TotalCachedTokens, &inv.TokenCalibrationRatio, &inv.SummaryJSON); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// ListRecentDoneInvestigations returns the most recent COMPLETED (done)
// investigations other than excludeID, newest first — the candidate pool for the
// AUTOMATIC host-scoped priors digest (done-only: a prior's value there is its
// final conclusion). summary_json is returned raw for the caller to parse.
func (s *Store) ListRecentDoneInvestigations(ctx context.Context, excludeID string, limit int) ([]PriorInvestigation, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, goal, status, created_at, allowed_hosts_json, summary_json
          FROM investigations
         WHERE status='done' AND id != ?
         ORDER BY created_at DESC LIMIT ?`, excludeID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPriorInvestigations(rows)
}

// ListRecentInvestigationsForPriors returns the most recent investigations of
// ANY status other than excludeID, newest first — the candidate pool for the
// operator's MANUAL prior picker, which may attach aborted/active runs (for
// their findings/partial evidence) in addition to done runs.
func (s *Store) ListRecentInvestigationsForPriors(ctx context.Context, excludeID string, limit int) ([]PriorInvestigation, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, goal, status, created_at, allowed_hosts_json, summary_json
          FROM investigations
         WHERE id != ?
         ORDER BY created_at DESC LIMIT ?`, excludeID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPriorInvestigations(rows)
}

// AppendMessage assigns the next seq for the investigation atomically.
func (s *Store) AppendMessage(ctx context.Context, m Message) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE investigation_id=?`, m.InvestigationID).
		Scan(&nextSeq); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
        INSERT INTO messages (investigation_id, seq, role, content, tool_call_id, tool_calls_json, timestamp, archived)
        VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		m.InvestigationID, nextSeq, m.Role, m.Content, m.ToolCallID, m.ToolCallsJSON, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) ListMessages(ctx context.Context, investigationID string, includeArchived bool) ([]Message, error) {
	q := `SELECT id, investigation_id, seq, role, content, tool_call_id, tool_calls_json, timestamp, archived
            FROM messages WHERE investigation_id=?`
	if !includeArchived {
		q += ` AND archived=0`
	}
	q += ` ORDER BY seq`
	rows, err := s.db.QueryContext(ctx, q, investigationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		var m Message
		var arch int
		if err := rows.Scan(&m.ID, &m.InvestigationID, &m.Seq, &m.Role, &m.Content, &m.ToolCallID, &m.ToolCallsJSON, &m.Timestamp, &arch); err != nil {
			return nil, err
		}
		m.Archived = arch == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) InsertToolCall(ctx context.Context, tc ToolCallRow) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO tool_calls (id, investigation_id, seq, tool, input_json, rationale, status, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tc.ID, tc.InvestigationID, tc.Seq, tc.Tool, tc.InputJSON, tc.Rationale, tc.Status, time.Now().UTC())
	return err
}

// SetToolCallInput overwrites input_json (used by operator edit-and-rerun).
func (s *Store) SetToolCallInput(ctx context.Context, id, newInputJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tool_calls SET input_json=? WHERE id=?`, newInputJSON, id)
	return err
}

// SetToolCallRationale overwrites the rationale string.
func (s *Store) SetToolCallRationale(ctx context.Context, id, rationale string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tool_calls SET rationale=? WHERE id=?`, rationale, id)
	return err
}

// SetToolCallBroadConfirmed flips the typed flag the broad-selector flow
// uses (review C1 — replaces a stringy marker that the model could forge).
func (s *Store) SetToolCallBroadConfirmed(ctx context.Context, id string, v bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tool_calls SET broad_confirmed=? WHERE id=?`, boolToInt(v), id)
	return err
}

// boolPtr is a Scan adapter that accepts SQLite INTEGER 0/1 as a Go bool.
type boolScanner struct{ dst *bool }

func (b boolScanner) Scan(src any) error {
	if src == nil {
		*b.dst = false
		return nil
	}
	switch v := src.(type) {
	case int64:
		*b.dst = v != 0
	case bool:
		*b.dst = v
	}
	return nil
}

func boolPtr(b *bool) any { return boolScanner{dst: b} }

func (s *Store) UpdateToolCall(ctx context.Context, id, status, decidedBy, taskID, resultJSON string) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE tool_calls SET status=?, decided_by=?, task_id=?, result_json=?, decided_at=?
         WHERE id=?`,
		status, nullable(decidedBy), nullable(taskID), nullable(resultJSON), time.Now().UTC(), id)
	return err
}

func (s *Store) GetToolCall(ctx context.Context, id string) (ToolCallRow, error) {
	var tc ToolCallRow
	err := s.db.QueryRowContext(ctx, `
        SELECT id, investigation_id, seq, tool, input_json, COALESCE(rationale,''),
               status, decided_by, task_id, created_at, decided_at, result_json, broad_confirmed
          FROM tool_calls WHERE id=?`, id).
		Scan(&tc.ID, &tc.InvestigationID, &tc.Seq, &tc.Tool, &tc.InputJSON, &tc.Rationale,
			&tc.Status, &tc.DecidedBy, &tc.TaskID, &tc.CreatedAt, &tc.DecidedAt, &tc.ResultJSON,
			boolPtr(&tc.BroadConfirmed))
	if errors.Is(err, sql.ErrNoRows) {
		return ToolCallRow{}, fmt.Errorf("tool_call %s not found", id)
	}
	return tc, err
}

func (s *Store) ListToolCalls(ctx context.Context, investigationID string) ([]ToolCallRow, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, investigation_id, seq, tool, input_json, COALESCE(rationale,''),
               status, decided_by, task_id, created_at, decided_at, result_json, broad_confirmed
          FROM tool_calls WHERE investigation_id=? ORDER BY seq`, investigationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ToolCallRow
	for rows.Next() {
		var tc ToolCallRow
		if err := rows.Scan(&tc.ID, &tc.InvestigationID, &tc.Seq, &tc.Tool, &tc.InputJSON, &tc.Rationale,
			&tc.Status, &tc.DecidedBy, &tc.TaskID, &tc.CreatedAt, &tc.DecidedAt, &tc.ResultJSON,
			boolPtr(&tc.BroadConfirmed)); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// PendingToolCall returns the current 'pending' tool call for an investigation,
// or nil if there is none. Used by the UI to render the operator-facing
// approve/skip card.
func (s *Store) PendingToolCall(ctx context.Context, investigationID string) (*ToolCallRow, error) {
	var tc ToolCallRow
	err := s.db.QueryRowContext(ctx, `
        SELECT id, investigation_id, seq, tool, input_json, COALESCE(rationale,''),
               status, decided_by, task_id, created_at, decided_at, result_json, broad_confirmed
          FROM tool_calls WHERE investigation_id=? AND status='pending'
          ORDER BY seq DESC LIMIT 1`, investigationID).
		Scan(&tc.ID, &tc.InvestigationID, &tc.Seq, &tc.Tool, &tc.InputJSON, &tc.Rationale,
			&tc.Status, &tc.DecidedBy, &tc.TaskID, &tc.CreatedAt, &tc.DecidedAt, &tc.ResultJSON,
			boolPtr(&tc.BroadConfirmed))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tc, nil
}

func (s *Store) AddFinding(ctx context.Context, f Finding) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO findings (id, investigation_id, severity, code, message, evidence_json, pinned, ignored, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.InvestigationID, f.Severity, f.Code, f.Message, f.EvidenceJSON,
		boolToInt(f.Pinned), boolToInt(f.Ignored), time.Now().UTC())
	return err
}

func (s *Store) ListFindings(ctx context.Context, investigationID string) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, investigation_id, severity, code, message, COALESCE(evidence_json,''),
               pinned, ignored, created_at
          FROM findings WHERE investigation_id=? ORDER BY pinned DESC, ignored ASC, created_at`, investigationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Finding
	for rows.Next() {
		var f Finding
		var pinned, ignored int
		if err := rows.Scan(&f.ID, &f.InvestigationID, &f.Severity, &f.Code, &f.Message, &f.EvidenceJSON,
			&pinned, &ignored, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.Pinned = pinned == 1
		f.Ignored = ignored == 1
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindingCounts is a per-severity tally for one investigation, used by the
// list view to render the mini-bar (NcNwNi…).
type FindingCounts struct {
	Critical int
	Error    int
	Warn     int
	Info     int
}

// FindingCountsByInvestigation returns severity buckets keyed by investigation
// id. Single GROUP BY query; ignored findings are excluded so they don't
// inflate the badge after the operator has dismissed them.
func (s *Store) FindingCountsByInvestigation(ctx context.Context) (map[string]FindingCounts, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT investigation_id, severity, COUNT(*)
          FROM findings
         WHERE ignored = 0
         GROUP BY investigation_id, severity`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]FindingCounts{}
	for rows.Next() {
		var inv, sev string
		var n int
		if err := rows.Scan(&inv, &sev, &n); err != nil {
			return nil, err
		}
		c := out[inv]
		switch sev {
		case "critical":
			c.Critical = n
		case "error":
			c.Error = n
		case "warn":
			c.Warn = n
		case "info":
			c.Info = n
		}
		out[inv] = c
	}
	return out, rows.Err()
}

// SnapshotCounters returns a small fingerprint used by SSE to decide when
// the page should self-refresh: status, tool_call count, last tool_call
// status, findings count, budget counters, updated_at, and terminal summary
// content for hashing — in one bounded query (review M8).
func (s *Store) SnapshotCounters(ctx context.Context, invID string) (status, lastTCStatus string, steps, findings int, updatedAt time.Time, promptTokens, completionTokens, totalToolCalls, extraSteps, extraTokens int, terminalSummary sql.NullString, err error) {
	err = s.db.QueryRowContext(ctx, `
        SELECT i.status,
               COALESCE((SELECT status FROM tool_calls WHERE investigation_id=i.id ORDER BY seq DESC LIMIT 1), ''),
               (SELECT COUNT(*) FROM tool_calls WHERE investigation_id=i.id),
               (SELECT COUNT(*) FROM findings   WHERE investigation_id=i.id),
               i.updated_at,
               i.total_prompt_tokens,
               i.total_completion_tokens,
               i.total_tool_calls,
               i.extra_steps,
               i.extra_tokens,
               i.summary_json
          FROM investigations i WHERE i.id=?`, invID).
		Scan(&status, &lastTCStatus, &steps, &findings, &updatedAt, &promptTokens, &completionTokens, &totalToolCalls, &extraSteps, &extraTokens, &terminalSummary)
	return
}

// MarkMessagesArchived flags every message in [investigationID, seq <= upToSeq]
// as archived. Subsequent ListMessages(.., includeArchived=false) skips them.
// Used by compaction (week 5).
func (s *Store) MarkMessagesArchived(ctx context.Context, investigationID string, upToSeq int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET archived=1 WHERE investigation_id=? AND seq<=?`,
		investigationID, upToSeq)
	return err
}

// SetFindingPinned and SetFindingIgnored toggle the corresponding flag for
// a finding. Used by the operator UI in week 4 to curate the memo.
func (s *Store) SetFindingPinned(ctx context.Context, id string, pinned bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE findings SET pinned=? WHERE id=?`, boolToInt(pinned), id)
	return err
}

func (s *Store) SetFindingIgnored(ctx context.Context, id string, ignored bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE findings SET ignored=? WHERE id=?`, boolToInt(ignored), id)
	return err
}

func (s *Store) GetFinding(ctx context.Context, id string) (Finding, error) {
	var f Finding
	var pinned, ignored int
	err := s.db.QueryRowContext(ctx, `
        SELECT id, investigation_id, severity, code, message, COALESCE(evidence_json,''),
               pinned, ignored, created_at
          FROM findings WHERE id=?`, id).
		Scan(&f.ID, &f.InvestigationID, &f.Severity, &f.Code, &f.Message, &f.EvidenceJSON,
			&pinned, &ignored, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Finding{}, fmt.Errorf("finding %s not found", id)
	}
	if err != nil {
		return Finding{}, err
	}
	f.Pinned = pinned == 1
	f.Ignored = ignored == 1
	return f, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
