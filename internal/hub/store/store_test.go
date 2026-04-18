package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrationsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	for i := 0; i < 3; i++ {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		_ = s.Close()
	}
}

func TestUpsertAndListHosts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	h := Host{
		ID:              "host-a",
		AgentVersion:    "0.1.0",
		Labels:          map[string]string{"env": "prod"},
		Facts:           map[string]string{"os": "linux"},
		CertFingerprint: "ab:cd",
		Status:          "online",
	}
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	first, _ := s.GetHost(ctx, "host-a")
	if first.Labels["env"] != "prod" {
		t.Fatalf("labels not stored: %+v", first)
	}

	// Re-upsert with new label; first_seen_at must be preserved.
	time.Sleep(5 * time.Millisecond)
	h.Labels["env"] = "stage"
	if err := s.UpsertHost(ctx, h); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	second, _ := s.GetHost(ctx, "host-a")
	if !second.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Fatalf("first_seen_at changed: %v vs %v", first.FirstSeenAt, second.FirstSeenAt)
	}
	if second.Labels["env"] != "stage" {
		t.Fatalf("label not updated")
	}

	list, err := s.ListHosts(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
}

func TestReplaceCollectorManifests(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.UpsertHost(ctx, Host{ID: "h1", Labels: map[string]string{}, Facts: map[string]string{}, CertFingerprint: "x", Status: "online"})

	mans := []CollectorManifest{
		{Name: "system_info", Version: "1.0.0", ManifestJSON: []byte(`{}`)},
		{Name: "net_listen", Version: "1.0.0", ManifestJSON: []byte(`{}`)},
	}
	if err := s.ReplaceCollectorManifests(ctx, "h1", mans); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := s.ListCollectorManifests(ctx, "h1")
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %v len=%d", err, len(got))
	}

	// Replace with a smaller set; the missing one must vanish.
	if err := s.ReplaceCollectorManifests(ctx, "h1", mans[:1]); err != nil {
		t.Fatalf("replace2: %v", err)
	}
	got, _ = s.ListCollectorManifests(ctx, "h1")
	if len(got) != 1 || got[0].Name != "system_info" {
		t.Fatalf("expected only system_info, got %+v", got)
	}
}

func TestBootstrapTokenLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if err := s.InsertBootstrapToken(ctx, "secret-a", "agent-1", "admin", time.Minute); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Wrong agent_id must fail (C2 — token bound to expected agent).
	if err := s.ConsumeBootstrapToken(ctx, "secret-a", "agent-evil"); err == nil {
		t.Fatal("expected mismatched agent_id to fail")
	}
	// Correct agent_id succeeds.
	if err := s.ConsumeBootstrapToken(ctx, "secret-a", "agent-1"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	// Replay must fail.
	if err := s.ConsumeBootstrapToken(ctx, "secret-a", "agent-1"); err == nil {
		t.Fatal("expected replay to fail")
	}
	// Wrong token must fail.
	if err := s.ConsumeBootstrapToken(ctx, "wrong", "agent-1"); err == nil {
		t.Fatal("expected wrong token to fail")
	}

	if err := s.InsertBootstrapToken(ctx, "secret-b", "agent-2", "admin", -1*time.Minute); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.ConsumeBootstrapToken(ctx, "secret-b", "agent-2"); err == nil {
		t.Fatal("expected expired token to fail")
	}

	// Empty agent_id at issue time is rejected.
	if err := s.InsertBootstrapToken(ctx, "secret-c", "", "admin", time.Minute); err == nil {
		t.Fatal("expected empty agent_id to be rejected")
	}
}

func TestIdentityLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Verify before enroll → not enrolled.
	if err := s.VerifyIdentity(ctx, "agent-1", "fp:1"); err != ErrIdentityNotEnrolled {
		t.Fatalf("expected ErrIdentityNotEnrolled, got %v", err)
	}

	// Register and verify.
	if err := s.RegisterIdentity(ctx, "agent-1", "fp:1", "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.VerifyIdentity(ctx, "agent-1", "fp:1"); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Wrong fingerprint for the same agent_id is rejected (C1).
	if err := s.VerifyIdentity(ctx, "agent-1", "fp:evil"); err != ErrFingerprintMismatch {
		t.Fatalf("expected ErrFingerprintMismatch, got %v", err)
	}

	// Re-register without revoke is rejected (C3).
	if err := s.RegisterIdentity(ctx, "agent-1", "fp:2", "admin"); err != ErrIdentityExists {
		t.Fatalf("expected ErrIdentityExists, got %v", err)
	}

	// Revoke and re-verify.
	if err := s.RevokeIdentity(ctx, "agent-1", "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := s.VerifyIdentity(ctx, "agent-1", "fp:1"); err != ErrIdentityRevoked {
		t.Fatalf("expected ErrIdentityRevoked, got %v", err)
	}

	// After revoke, re-register MUST succeed (with a new fingerprint).
	if err := s.RegisterIdentity(ctx, "agent-1", "fp:rotated", "admin"); err != nil {
		t.Fatalf("register after revoke: %v", err)
	}
	if err := s.VerifyIdentity(ctx, "agent-1", "fp:rotated"); err != nil {
		t.Fatalf("verify after re-register: %v", err)
	}
	// Old fingerprint is dead.
	if err := s.VerifyIdentity(ctx, "agent-1", "fp:1"); err != ErrFingerprintMismatch {
		t.Fatalf("expected old fp dead, got %v", err)
	}
}

func TestAuditLog(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.AuditLog(ctx, "admin", "enroll", map[string]any{"agent_id": "h1"})
	entries, err := s.ListAudit(ctx, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v len=%d", err, len(entries))
	}
	if entries[0].Action != "enroll" || entries[0].Details["agent_id"] != "h1" {
		t.Fatalf("unexpected entry: %+v", entries[0])
	}
}
