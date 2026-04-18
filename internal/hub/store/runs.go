package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Run struct {
	ID              string
	InvestigationID sql.NullString
	Name            string
	Selector        map[string]string
	CreatedBy       string
	CreatedAt       time.Time
	FinishedAt      sql.NullTime
	Status          string
}

type Task struct {
	ID         string
	RunID      string
	HostID     string
	Collector  string
	Params     map[string]string
	Status     string
	StartedAt  sql.NullTime
	FinishedAt sql.NullTime
	DurationMs sql.NullInt64
	Error      string
}

type Result struct {
	TaskID      string
	DataJSON    []byte
	HintsJSON   []byte
	Stderr      string
	ExitCode    int
	ArtifactDir string
}

func (s *Store) InsertRun(ctx context.Context, r Run) error {
	sel, _ := json.Marshal(r.Selector)
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO runs (id, investigation_id, name, selector_json, created_by, created_at, status)
        VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, nullString(r.InvestigationID), r.Name, string(sel), r.CreatedBy, time.Now().UTC(), r.Status)
	return err
}

func (s *Store) FinishRun(ctx context.Context, runID, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET finished_at=?, status=? WHERE id=?`,
		time.Now().UTC(), status, runID)
	return err
}

func (s *Store) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, investigation_id, COALESCE(name,''), COALESCE(selector_json,''),
               COALESCE(created_by,''), created_at, finished_at, status
          FROM runs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		var r Run
		var sel string
		if err := rows.Scan(&r.ID, &r.InvestigationID, &r.Name, &sel, &r.CreatedBy, &r.CreatedAt, &r.FinishedAt, &r.Status); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(sel), &r.Selector)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	var r Run
	var sel string
	err := s.db.QueryRowContext(ctx, `
        SELECT id, investigation_id, COALESCE(name,''), COALESCE(selector_json,''),
               COALESCE(created_by,''), created_at, finished_at, status
          FROM runs WHERE id=?`, id).
		Scan(&r.ID, &r.InvestigationID, &r.Name, &sel, &r.CreatedBy, &r.CreatedAt, &r.FinishedAt, &r.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("run %s not found", id)
	}
	if err != nil {
		return Run{}, err
	}
	_ = json.Unmarshal([]byte(sel), &r.Selector)
	return r, nil
}

func (s *Store) InsertTask(ctx context.Context, t Task) error {
	params, _ := json.Marshal(t.Params)
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO tasks (id, run_id, host_id, collector, params_json, status)
        VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.RunID, t.HostID, t.Collector, string(params), t.Status)
	return err
}

func (s *Store) StartTask(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status='sent', started_at=? WHERE id=? AND status='pending'`,
		time.Now().UTC(), id)
	return err
}

func (s *Store) FinishTask(ctx context.Context, id, status string, durMs int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status=?, finished_at=?, duration_ms=?, error=? WHERE id=?`,
		status, time.Now().UTC(), durMs, errMsg, id)
	return err
}

// GetTask returns a single task by id. Cheap O(1) — added to avoid the
// O(n_runs × n_tasks) walk that the investigator was doing in week 3.
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	var t Task
	var params string
	err := s.db.QueryRowContext(ctx, `
        SELECT id, run_id, host_id, collector, COALESCE(params_json,''), status,
               started_at, finished_at, duration_ms, COALESCE(error,'')
          FROM tasks WHERE id=?`, id).
		Scan(&t.ID, &t.RunID, &t.HostID, &t.Collector, &params, &t.Status,
			&t.StartedAt, &t.FinishedAt, &t.DurationMs, &t.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if err != nil {
		return Task{}, err
	}
	_ = json.Unmarshal([]byte(params), &t.Params)
	return t, nil
}

func (s *Store) ListTasks(ctx context.Context, runID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, run_id, host_id, collector, COALESCE(params_json,''), status,
               started_at, finished_at, duration_ms, COALESCE(error,'')
          FROM tasks WHERE run_id=? ORDER BY host_id`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Task
	for rows.Next() {
		var t Task
		var params string
		if err := rows.Scan(&t.ID, &t.RunID, &t.HostID, &t.Collector, &params, &t.Status,
			&t.StartedAt, &t.FinishedAt, &t.DurationMs, &t.Error); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(params), &t.Params)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpsertResult(ctx context.Context, r Result) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO results (task_id, data_json, hints_json, stderr, exit_code, artifact_dir)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(task_id) DO UPDATE SET
            data_json    = excluded.data_json,
            hints_json   = excluded.hints_json,
            stderr       = excluded.stderr,
            exit_code    = excluded.exit_code,
            artifact_dir = excluded.artifact_dir`,
		r.TaskID, string(r.DataJSON), string(r.HintsJSON), r.Stderr, r.ExitCode, r.ArtifactDir)
	return err
}

func (s *Store) GetResult(ctx context.Context, taskID string) (Result, error) {
	var r Result
	var data, hints string
	err := s.db.QueryRowContext(ctx, `
        SELECT task_id, COALESCE(data_json,''), COALESCE(hints_json,''),
               COALESCE(stderr,''), COALESCE(exit_code,0), COALESCE(artifact_dir,'')
          FROM results WHERE task_id=?`, taskID).
		Scan(&r.TaskID, &data, &hints, &r.Stderr, &r.ExitCode, &r.ArtifactDir)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, sql.ErrNoRows
	}
	if err != nil {
		return Result{}, err
	}
	r.DataJSON = []byte(data)
	r.HintsJSON = []byte(hints)
	return r, nil
}

func nullString(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}
