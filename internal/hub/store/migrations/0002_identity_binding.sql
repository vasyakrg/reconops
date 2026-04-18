-- Bind every bootstrap token to a single expected agent_id at issue time.
-- Tokens issued before this migration cannot be consumed (column NULL → no
-- match). MVP-fresh install has no rows so no backfill is needed.
ALTER TABLE bootstrap_tokens ADD COLUMN expected_agent_id TEXT;

-- Track explicit revocation of agent identities. Connect rejects any peer
-- whose (agent_id, fingerprint) is not present here without a revoked_at.
CREATE TABLE IF NOT EXISTS enrolled_identities (
    agent_id          TEXT PRIMARY KEY,
    cert_fingerprint  TEXT NOT NULL,
    enrolled_at       DATETIME NOT NULL,
    enrolled_via      TEXT,         -- token issuer
    revoked_at        DATETIME,
    revoked_reason    TEXT
);

CREATE INDEX IF NOT EXISTS enrolled_identities_fp_idx ON enrolled_identities(cert_fingerprint);
