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
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, actor, action, COALESCE(details_json,'') FROM audit ORDER BY id DESC LIMIT ?`, limit)
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
