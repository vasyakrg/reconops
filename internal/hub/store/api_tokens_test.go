package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAPIToken_GenerateRoundTrip(t *testing.T) {
	raw, hash, prefix, err := GenerateAPIToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(raw, APITokenPrefix()) {
		t.Fatalf("raw missing prefix: %q", raw)
	}
	if HashAPIToken(raw) != hash {
		t.Fatalf("hash mismatch")
	}
	if !strings.HasPrefix(prefix, APITokenPrefix()) {
		t.Fatalf("prefix missing marker: %q", prefix)
	}
}

func TestAPIToken_InsertLookupTouch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	raw, hash, prefix, err := GenerateAPIToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	id, err := s.InsertAPIToken(ctx, "ci", hash, prefix, string(APIScopeInvestigate), "operator", sql.NullTime{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}

	tok, err := s.LookupAPIToken(ctx, raw)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if tok.ID != id || tok.Scope != APIScopeInvestigate || tok.Name != "ci" {
		t.Fatalf("unexpected row: %+v", tok)
	}
	if tok.LastUsedAt.Valid {
		t.Fatalf("last_used_at should be null before Touch")
	}
	if err := s.TouchAPIToken(ctx, id); err != nil {
		t.Fatalf("touch: %v", err)
	}
	tok2, err := s.LookupAPIToken(ctx, raw)
	if err != nil {
		t.Fatalf("lookup after touch: %v", err)
	}
	if !tok2.LastUsedAt.Valid {
		t.Fatalf("last_used_at not set after touch")
	}
}

func TestAPIToken_Revoked(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	raw, hash, prefix, _ := GenerateAPIToken()
	id, err := s.InsertAPIToken(ctx, "x", hash, prefix, string(APIScopeRead), "op", sql.NullTime{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIToken(ctx, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = s.LookupAPIToken(ctx, raw)
	if !errors.Is(err, ErrAPITokenInvalid) {
		t.Fatalf("want ErrAPITokenInvalid after revoke, got %v", err)
	}
}

func TestAPIToken_Expired(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	raw, hash, prefix, _ := GenerateAPIToken()
	past := sql.NullTime{Time: time.Now().UTC().Add(-time.Hour), Valid: true}
	if _, err := s.InsertAPIToken(ctx, "x", hash, prefix, string(APIScopeRead), "op", past); err != nil {
		t.Fatal(err)
	}
	_, err := s.LookupAPIToken(ctx, raw)
	if !errors.Is(err, ErrAPITokenInvalid) {
		t.Fatalf("want ErrAPITokenInvalid for expired, got %v", err)
	}
}

func TestAPIToken_InvalidScope(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_, hash, prefix, _ := GenerateAPIToken()
	_, err := s.InsertAPIToken(ctx, "x", hash, prefix, "root", "op", sql.NullTime{})
	if err == nil {
		t.Fatal("expected error on invalid scope")
	}
}

func TestAPIToken_List(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, hash, prefix, _ := GenerateAPIToken()
		if _, err := s.InsertAPIToken(ctx, "n", hash, prefix, string(APIScopeRead), "op", sql.NullTime{}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListAPITokens(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3, got %d", len(rows))
	}
}
