-- Week 1 schema: hosts inventory + audit log.
-- Other tables (runs/tasks/results/investigations/...) land in later migrations.

CREATE TABLE IF NOT EXISTS hosts (
    id                TEXT PRIMARY KEY,
    agent_version     TEXT,
    labels_json       TEXT NOT NULL,
    facts_json        TEXT NOT NULL,
    cert_fingerprint  TEXT NOT NULL,
    first_seen_at     DATETIME NOT NULL,
    last_seen_at      DATETIME NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('online','offline','degraded'))
);

CREATE INDEX IF NOT EXISTS hosts_status_idx ON hosts(status);

CREATE TABLE IF NOT EXISTS collector_manifests (
    host_id        TEXT NOT NULL,
    name           TEXT NOT NULL,
    version        TEXT NOT NULL,
    manifest_json  TEXT NOT NULL,
    PRIMARY KEY (host_id, name),
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS bootstrap_tokens (
    token_hash   TEXT PRIMARY KEY, -- sha256 of the plaintext token
    issued_at    DATETIME NOT NULL,
    expires_at   DATETIME NOT NULL,
    consumed_at  DATETIME,
    issued_by    TEXT,
    used_by      TEXT
);

CREATE INDEX IF NOT EXISTS bootstrap_tokens_expires_idx ON bootstrap_tokens(expires_at);

CREATE TABLE IF NOT EXISTS audit (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            DATETIME NOT NULL,
    actor         TEXT NOT NULL,
    action        TEXT NOT NULL,
    details_json  TEXT
);

CREATE INDEX IF NOT EXISTS audit_ts_idx ON audit(ts);
