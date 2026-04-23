package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// APITokenScope enumerates the allowed scopes on an api_tokens row.
// Kept as a small string type so handlers can switch cleanly on it.
type APITokenScope string

const (
	APIScopeRead        APITokenScope = "read"
	APIScopeInvestigate APITokenScope = "investigate"
	APIScopeAdmin       APITokenScope = "admin"

	apiTokenPrefix = "recon_pat_"
)

// ErrAPITokenInvalid is returned for any lookup failure — unknown hash,
// revoked, expired. The exact reason is intentionally not leaked (the
// middleware maps all three to 401 anyway).
var ErrAPITokenInvalid = errors.New("api token invalid, revoked, or expired")

// ValidAPIScope reports whether s is one of the three allowed scopes.
func ValidAPIScope(s string) bool {
	switch APITokenScope(s) {
	case APIScopeRead, APIScopeInvestigate, APIScopeAdmin:
		return true
	}
	return false
}

// APIToken is the full-row view returned by lookup. Raw token value is
// never persisted; it exists only at issue time and is returned once to
// the caller.
type APIToken struct {
	ID         string
	Name       string
	TokenHash  string
	Prefix     string
	Scope      APITokenScope
	CreatedBy  string
	CreatedAt  time.Time
	LastUsedAt sql.NullTime
	ExpiresAt  sql.NullTime
	RevokedAt  sql.NullTime
}

// GenerateAPIToken returns a new raw token string (with the recon_pat_
// prefix) and its sha256 hash. Raw value must be shown to the operator
// once and then discarded — only the hash is stored.
func GenerateAPIToken() (raw, hash, prefix string, err error) {
	var buf [32]byte
	if _, err = rand.Read(buf[:]); err != nil {
		return "", "", "", fmt.Errorf("generate api token: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(buf[:])
	raw = apiTokenPrefix + body
	sum := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(sum[:])
	// Prefix covers the "recon_pat_" marker + first two body chars, enough
	// for visual identification in the UI without disclosing the secret.
	if len(raw) >= len(apiTokenPrefix)+2 {
		prefix = raw[:len(apiTokenPrefix)+2]
	} else {
		prefix = raw
	}
	return raw, hash, prefix, nil
}

// HashAPIToken hashes a raw token the same way GenerateAPIToken does so
// the middleware can look it up.
func HashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NewULID returns a monotonic-enough ULID-like identifier for api_tokens.id.
// We already have modernc.org/sqlite and don't want a new dep just for ULID,
// so we derive a 26-char base32-ish id from time + random bytes.
func newAPITokenID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	// Prepend seconds-since-epoch in big-endian to the first 6 bytes so IDs
	// sort approximately by creation time without a real ULID lib.
	ts := uint64(time.Now().Unix())
	buf[0] = byte(ts >> 40)
	buf[1] = byte(ts >> 32)
	buf[2] = byte(ts >> 24)
	buf[3] = byte(ts >> 16)
	buf[4] = byte(ts >> 8)
	buf[5] = byte(ts)
	return hex.EncodeToString(buf[:])
}

// InsertAPIToken creates a new token row. Raw token value must be kept by
// the caller and shown to the operator only once.
func (s *Store) InsertAPIToken(ctx context.Context, name, hash, prefix, scope, createdBy string, expiresAt sql.NullTime) (id string, err error) {
	if name == "" {
		return "", errors.New("name required")
	}
	if !ValidAPIScope(scope) {
		return "", fmt.Errorf("invalid scope %q", scope)
	}
	if hash == "" || prefix == "" {
		return "", errors.New("hash and prefix required")
	}
	id = newAPITokenID()
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO api_tokens (id, name, token_hash, prefix, scope, created_by, created_at, expires_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, hash, prefix, scope, createdBy, time.Now().UTC(), expiresAt)
	if err != nil {
		return "", fmt.Errorf("insert api_token: %w", err)
	}
	return id, nil
}

// LookupAPIToken resolves a raw token to a live, non-revoked, non-expired
// row. Returns ErrAPITokenInvalid on any miss. Caller should immediately
// call TouchAPIToken to update last_used_at.
func (s *Store) LookupAPIToken(ctx context.Context, raw string) (*APIToken, error) {
	hash := HashAPIToken(raw)
	row := s.db.QueryRowContext(ctx, `
        SELECT id, name, token_hash, prefix, scope, created_by, created_at,
               last_used_at, expires_at, revoked_at
          FROM api_tokens
         WHERE token_hash = ?`, hash)
	var t APIToken
	var scope string
	if err := row.Scan(&t.ID, &t.Name, &t.TokenHash, &t.Prefix, &scope, &t.CreatedBy,
		&t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAPITokenInvalid
		}
		return nil, fmt.Errorf("lookup api_token: %w", err)
	}
	t.Scope = APITokenScope(scope)
	if t.RevokedAt.Valid {
		return nil, ErrAPITokenInvalid
	}
	if t.ExpiresAt.Valid && time.Now().UTC().After(t.ExpiresAt.Time) {
		return nil, ErrAPITokenInvalid
	}
	return &t, nil
}

// TouchAPIToken bumps last_used_at. Best-effort: errors are logged by the
// caller but never block the request — the token is already authenticated.
func (s *Store) TouchAPIToken(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC(), id)
	return err
}

// ListAPITokens returns all tokens (including revoked / expired) for the
// settings UI, newest first. Raw values are not stored, so only prefix +
// metadata is shown.
func (s *Store) ListAPITokens(ctx context.Context, limit int) ([]APIToken, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, name, token_hash, prefix, scope, created_by, created_at,
               last_used_at, expires_at, revoked_at
          FROM api_tokens
         ORDER BY created_at DESC
         LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var scope string
		if err := rows.Scan(&t.ID, &t.Name, &t.TokenHash, &t.Prefix, &scope, &t.CreatedBy,
			&t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		t.Scope = APITokenScope(scope)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken sets revoked_at. Idempotent — a second call on an already-
// revoked row is a no-op. The row stays (audit trail); middleware rejects it.
func (s *Store) RevokeAPIToken(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC(), id)
	return err
}

// APITokenPrefix is exported for handlers that need to check whether a raw
// Authorization value came from our issuer before attempting a lookup.
func APITokenPrefix() string { return apiTokenPrefix }
