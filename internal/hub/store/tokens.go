package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type BootstrapToken struct {
	TokenHash       string
	ExpectedAgentID string
	IssuedAt        time.Time
	ExpiresAt       time.Time
	ConsumedAt      sql.NullTime
	IssuedBy        string
	UsedBy          string
}

func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// InsertBootstrapToken stores a freshly-generated token bound to a single
// agent_id. Consume rejects any token used for a different agent_id (C2).
func (s *Store) InsertBootstrapToken(ctx context.Context, plaintext, expectedAgentID, issuedBy string, ttl time.Duration) error {
	if expectedAgentID == "" {
		return errors.New("expected_agent_id required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO bootstrap_tokens (token_hash, expected_agent_id, issued_at, expires_at, issued_by)
        VALUES (?, ?, ?, ?, ?)`,
		HashToken(plaintext), expectedAgentID, now, now.Add(ttl), issuedBy)
	return err
}

// ErrTokenInvalid is returned by ConsumeBootstrapToken for any failure
// reason — unknown hash, expired, already consumed, or agent_id mismatch.
// The exact reason is intentionally not leaked to the caller.
var ErrTokenInvalid = errors.New("bootstrap token invalid, expired, or bound to a different agent")

// ConsumeBootstrapToken atomically validates and marks the token consumed.
// The token must (a) exist, (b) not be expired, (c) not be consumed, and
// (d) be bound to the same expected_agent_id supplied here.
func (s *Store) ConsumeBootstrapToken(ctx context.Context, plaintext, agentID string) error {
	hash := HashToken(plaintext)
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
        UPDATE bootstrap_tokens
           SET consumed_at = ?, used_by = ?
         WHERE token_hash = ?
           AND consumed_at IS NULL
           AND expires_at > ?
           AND expected_agent_id = ?`,
		now, agentID, hash, now, agentID)
	if err != nil {
		return fmt.Errorf("consume token: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrTokenInvalid
	}
	return nil
}
