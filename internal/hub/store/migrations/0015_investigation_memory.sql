CREATE TABLE IF NOT EXISTS investigation_memory (
    id                 TEXT PRIMARY KEY,
    investigation_id   TEXT NOT NULL REFERENCES investigations(id) ON DELETE CASCADE,
    kind               TEXT NOT NULL,
    content            TEXT NOT NULL,
    evidence_refs_json TEXT NOT NULL DEFAULT '[]',
    message_seq_start  INTEGER NOT NULL DEFAULT 0,
    message_seq_end    INTEGER NOT NULL DEFAULT 0,
    token_estimate     INTEGER NOT NULL DEFAULT 0,
    created_at         DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_investigation_memory_inv_created
    ON investigation_memory(investigation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_investigation_memory_inv_kind
    ON investigation_memory(investigation_id, kind);

