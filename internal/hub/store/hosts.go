package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Host struct {
	ID              string
	AgentVersion    string
	Labels          map[string]string
	Facts           map[string]string
	CertFingerprint string
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	Status          string
}

type CollectorManifest struct {
	HostID       string
	Name         string
	Version      string
	ManifestJSON []byte
}

// UpsertHost inserts or updates a host on Hello / re-Hello. first_seen_at is
// preserved across upserts.
func (s *Store) UpsertHost(ctx context.Context, h Host) error {
	labels, err := json.Marshal(h.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	facts, err := json.Marshal(h.Facts)
	if err != nil {
		return fmt.Errorf("marshal facts: %w", err)
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO hosts (id, agent_version, labels_json, facts_json, cert_fingerprint, first_seen_at, last_seen_at, status)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            agent_version = excluded.agent_version,
            labels_json   = excluded.labels_json,
            facts_json    = excluded.facts_json,
            cert_fingerprint = excluded.cert_fingerprint,
            last_seen_at  = excluded.last_seen_at,
            status        = excluded.status
    `, h.ID, h.AgentVersion, string(labels), string(facts), h.CertFingerprint, now, now, h.Status)
	if err != nil {
		return fmt.Errorf("upsert host: %w", err)
	}
	return nil
}

// TouchHost updates last_seen_at + status; called on Heartbeat.
func (s *Store) TouchHost(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE hosts SET last_seen_at=?, status=? WHERE id=?`, time.Now().UTC(), status, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("host not found")
	}
	return nil
}

func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, COALESCE(agent_version,''), labels_json, facts_json, cert_fingerprint, first_seen_at, last_seen_at, status
        FROM hosts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Host
	for rows.Next() {
		var h Host
		var labels, facts string
		if err := rows.Scan(&h.ID, &h.AgentVersion, &labels, &facts, &h.CertFingerprint, &h.FirstSeenAt, &h.LastSeenAt, &h.Status); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(labels), &h.Labels)
		_ = json.Unmarshal([]byte(facts), &h.Facts)
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) GetHost(ctx context.Context, id string) (Host, error) {
	var h Host
	var labels, facts string
	err := s.db.QueryRowContext(ctx, `
        SELECT id, COALESCE(agent_version,''), labels_json, facts_json, cert_fingerprint, first_seen_at, last_seen_at, status
        FROM hosts WHERE id=?`, id).
		Scan(&h.ID, &h.AgentVersion, &labels, &facts, &h.CertFingerprint, &h.FirstSeenAt, &h.LastSeenAt, &h.Status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Host{}, fmt.Errorf("host %s: %w", id, err)
		}
		return Host{}, err
	}
	_ = json.Unmarshal([]byte(labels), &h.Labels)
	_ = json.Unmarshal([]byte(facts), &h.Facts)
	return h, nil
}

// ReplaceCollectorManifests atomically replaces the full set of manifests for
// a host. Called on every Hello so that disappearing collectors are removed.
func (s *Store) ReplaceCollectorManifests(ctx context.Context, hostID string, manifests []CollectorManifest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM collector_manifests WHERE host_id=?`, hostID); err != nil {
		return fmt.Errorf("clear manifests: %w", err)
	}
	for _, m := range manifests {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO collector_manifests (host_id, name, version, manifest_json)
            VALUES (?, ?, ?, ?)`, hostID, m.Name, m.Version, string(m.ManifestJSON)); err != nil {
			return fmt.Errorf("insert manifest %s: %w", m.Name, err)
		}
	}
	return tx.Commit()
}

func (s *Store) ListCollectorManifests(ctx context.Context, hostID string) ([]CollectorManifest, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT host_id, name, version, manifest_json FROM collector_manifests
        WHERE host_id=? ORDER BY name`, hostID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CollectorManifest
	for rows.Next() {
		var m CollectorManifest
		var body string
		if err := rows.Scan(&m.HostID, &m.Name, &m.Version, &body); err != nil {
			return nil, err
		}
		m.ManifestJSON = []byte(body)
		out = append(out, m)
	}
	return out, rows.Err()
}
