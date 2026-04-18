package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrIdentityExists      = errors.New("agent identity already enrolled")
	ErrIdentityNotEnrolled = errors.New("agent identity not enrolled")
	ErrIdentityRevoked     = errors.New("agent identity revoked")
	ErrFingerprintMismatch = errors.New("cert fingerprint does not match enrolled identity")
)

type EnrolledIdentity struct {
	AgentID         string
	CertFingerprint string
	EnrolledAt      time.Time
	EnrolledVia     string
	RevokedAt       sql.NullTime
	RevokedReason   string
}

// RegisterIdentity inserts a freshly-enrolled (agent_id, fingerprint) pair,
// or replaces a previously-revoked row for the same agent_id (revoke is the
// only legitimate way to allow re-enrollment under the same id). Returns
// ErrIdentityExists if a non-revoked row already exists.
func (s *Store) RegisterIdentity(ctx context.Context, agentID, fingerprint, via string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	var revokedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT cert_fingerprint, revoked_at FROM enrolled_identities WHERE agent_id=?`, agentID,
	).Scan(&existing, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// fall through to INSERT
	case err != nil:
		return err
	case !revokedAt.Valid:
		return ErrIdentityExists
	default:
		// Existing row is revoked — drop it so the INSERT below succeeds.
		if _, err := tx.ExecContext(ctx, `DELETE FROM enrolled_identities WHERE agent_id=?`, agentID); err != nil {
			return fmt.Errorf("clear revoked identity: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
        INSERT INTO enrolled_identities (agent_id, cert_fingerprint, enrolled_at, enrolled_via)
        VALUES (?, ?, ?, ?)`,
		agentID, fingerprint, time.Now().UTC(), via); err != nil {
		return fmt.Errorf("register identity: %w", err)
	}
	return tx.Commit()
}

// VerifyIdentity returns nil iff (agent_id, fingerprint) matches an enrolled,
// non-revoked identity. Used by Connect on every session.
func (s *Store) VerifyIdentity(ctx context.Context, agentID, fingerprint string) error {
	var fp string
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
        SELECT cert_fingerprint, revoked_at
          FROM enrolled_identities WHERE agent_id=?`, agentID).
		Scan(&fp, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrIdentityNotEnrolled
	}
	if err != nil {
		return err
	}
	if revokedAt.Valid {
		return ErrIdentityRevoked
	}
	if fp != fingerprint {
		return ErrFingerprintMismatch
	}
	return nil
}

// RevokeIdentity marks an identity revoked. Idempotent.
func (s *Store) RevokeIdentity(ctx context.Context, agentID, reason string) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE enrolled_identities SET revoked_at=?, revoked_reason=?
         WHERE agent_id=? AND revoked_at IS NULL`,
		time.Now().UTC(), reason, agentID)
	return err
}
