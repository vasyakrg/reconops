package store

import (
	"context"
	"encoding/json"
	"time"
)

type AuditEntry struct {
	ID      int64
	TS      time.Time
	Actor   string
	Action  string
	Details map[string]any
}

func (s *Store) AuditLog(ctx context.Context, actor, action string, details map[string]any) error {
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO audit (ts, actor, action, details_json) VALUES (?, ?, ?, ?)`,
		time.Now().UTC(), actor, action, string(body))
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	return s.ListAuditFiltered(ctx, "", "", limit)
}

// ListAuditFiltered narrows by actor and/or action substring (LIKE).
func (s *Store) ListAuditFiltered(ctx context.Context, actor, action string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id, ts, actor, action, COALESCE(details_json,'') FROM audit WHERE 1=1`
	args := []any{}
	if actor != "" {
		q += ` AND actor LIKE ?`
		args = append(args, "%"+actor+"%")
	}
	if action != "" {
		q += ` AND action LIKE ?`
		args = append(args, "%"+action+"%")
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var body string
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.Action, &body); err != nil {
			return nil, err
		}
		if body != "" {
			_ = json.Unmarshal([]byte(body), &e.Details)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
