package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SessionRow is the minimal session shape used by the web layer.
// Authentication state only — flash messages and login-throttle counters
// stay in memory.
type SessionRow struct {
	SID       string
	Username  string
	CSRF      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ErrSessionNotFound is returned when the sid is unknown OR the row exists
// but has already expired (treated as not-found so callers don't have to
// re-check the timestamp).
var ErrSessionNotFound = errors.New("session not found or expired")

// InsertSession writes a new session to the DB.
func (s *Store) InsertSession(ctx context.Context, r SessionRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO web_sessions (sid, username, csrf_token, created_at, expires_at)
                 VALUES (?, ?, ?, ?, ?)`,
		r.SID, r.Username, r.CSRF, r.CreatedAt.UTC(), r.ExpiresAt.UTC())
	return err
}

// GetSession returns the row for sid, or ErrSessionNotFound if missing /
// expired. Expired rows are not deleted here — DeleteExpiredSessions runs
// out of band on a timer.
func (s *Store) GetSession(ctx context.Context, sid string) (SessionRow, error) {
	var r SessionRow
	err := s.db.QueryRowContext(ctx,
		`SELECT sid, username, csrf_token, created_at, expires_at
                   FROM web_sessions WHERE sid = ?`, sid).
		Scan(&r.SID, &r.Username, &r.CSRF, &r.CreatedAt, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, ErrSessionNotFound
	}
	if err != nil {
		return SessionRow{}, err
	}
	if time.Now().After(r.ExpiresAt) {
		return SessionRow{}, ErrSessionNotFound
	}
	return r, nil
}

// DeleteSession removes the row by sid. Idempotent.
func (s *Store) DeleteSession(ctx context.Context, sid string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE sid = ?`, sid)
	return err
}

// DeleteExpiredSessions wipes any session past its expires_at. Called from
// the GC ticker — bounded growth.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM web_sessions WHERE expires_at <= ?`, time.Now().UTC())
	return err
}
