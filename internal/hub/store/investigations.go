package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Investigation struct {
	ID                    string
	Goal                  string
	Status                string // active|waiting|done|aborted
	CreatedBy             string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	Model                 string
	BaseURL               string
	TotalPromptTokens     int
	TotalCompletionTokens int
	TotalToolCalls        int
	CompactionTokens      int // tokens spent on internal compaction calls (review C2)
	SummaryJSON           sql.NullString
	// AllowedHosts: empty means "all hosts" (legacy behaviour). When set,
	// list_hosts only surfaces these and collect/collect_batch reject any
	// host_id outside the list.
	AllowedHosts []string
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
          (id, goal, status, created_by, created_at, updated_at, model, base_url, allowed_hosts_json)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.Goal, inv.Status, inv.CreatedBy, now, now, inv.Model, inv.BaseURL, allowed)
	return err
}

func (s *Store) GetInvestigation(ctx context.Context, id string) (Investigation, error) {
	var inv Investigation
	var allowed sql.NullString
	err := s.db.QueryRowContext(ctx, `
        SELECT id, goal, status, created_by, created_at, updated_at, model, base_url,
               total_prompt_tokens, total_completion_tokens, total_tool_calls, compaction_tokens, summary_json,
               allowed_hosts_json
          FROM investigations WHERE id=?`, id).
		Scan(&inv.ID, &inv.Goal, &inv.Status, &inv.CreatedBy, &inv.CreatedAt, &inv.UpdatedAt,
			&inv.Model, &inv.BaseURL,
			&inv.TotalPromptTokens, &inv.TotalCompletionTokens, &inv.TotalToolCalls, &inv.CompactionTokens, &inv.SummaryJSON,
			&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return Investigation{}, fmt.Errorf("investigation %s not found", id)
	}
	if allowed.Valid && allowed.String != "" {
		_ = json.Unmarshal([]byte(allowed.String), &inv.AllowedHosts)
	}
	return inv, err
}

func (s *Store) UpdateInvestigationStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE investigations SET status=?, updated_at=? WHERE id=?`,
		status, time.Now().UTC(), id)
	return err
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
               total_prompt_tokens, total_completion_tokens, total_tool_calls, compaction_tokens, summary_json
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
			&inv.TotalPromptTokens, &inv.TotalCompletionTokens, &inv.TotalToolCalls, &inv.CompactionTokens, &inv.SummaryJSON); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
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
// status, and findings count — in one query (review M8).
func (s *Store) SnapshotCounters(ctx context.Context, invID string) (status, lastTCStatus string, steps, findings int, err error) {
	err = s.db.QueryRowContext(ctx, `
        SELECT i.status,
               COALESCE((SELECT status FROM tool_calls WHERE investigation_id=i.id ORDER BY seq DESC LIMIT 1), ''),
               (SELECT COUNT(*) FROM tool_calls WHERE investigation_id=i.id),
               (SELECT COUNT(*) FROM findings   WHERE investigation_id=i.id)
          FROM investigations i WHERE i.id=?`, invID).
		Scan(&status, &lastTCStatus, &steps, &findings)
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
