CREATE TABLE IF NOT EXISTS investigation_context_turns (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    investigation_id           TEXT NOT NULL REFERENCES investigations(id) ON DELETE CASCADE,
    message_seq                INTEGER NOT NULL DEFAULT 0,
    operation                  TEXT NOT NULL,
    model_profile              TEXT NOT NULL,
    estimated_prompt_tokens    INTEGER NOT NULL DEFAULT 0,
    provider_prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    provider_completion_tokens INTEGER NOT NULL DEFAULT 0,
    context_window_tokens      INTEGER NOT NULL DEFAULT 0,
    threshold_tokens           INTEGER NOT NULL DEFAULT 0,
    reserved_output_tokens     INTEGER NOT NULL DEFAULT 0,
    safety_headroom_tokens     INTEGER NOT NULL DEFAULT 0,
    should_compact             INTEGER NOT NULL DEFAULT 0,
    compaction_reason          TEXT,
    created_at                 DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_investigation_context_turns_inv_created
    ON investigation_context_turns(investigation_id, created_at DESC);

