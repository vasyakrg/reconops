-- Investigator schema (PROJECT.md §6).

CREATE TABLE IF NOT EXISTS investigations (
    id                TEXT PRIMARY KEY,
    goal              TEXT NOT NULL,
    status            TEXT NOT NULL,         -- active | waiting | done | aborted
    created_by        TEXT NOT NULL,
    created_at        DATETIME NOT NULL,
    updated_at        DATETIME NOT NULL,
    model             TEXT NOT NULL,
    base_url          TEXT NOT NULL,
    total_prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    total_completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tool_calls  INTEGER NOT NULL DEFAULT 0,
    summary_json      TEXT
);

CREATE INDEX IF NOT EXISTS investigations_status_idx ON investigations(status);

CREATE TABLE IF NOT EXISTS messages (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    investigation_id  TEXT NOT NULL,
    seq               INTEGER NOT NULL,      -- monotonic order within investigation
    role              TEXT NOT NULL,         -- system | user | assistant | tool | system_note
    content           TEXT NOT NULL,         -- raw content or JSON envelope
    tool_call_id      TEXT,                  -- non-null for role='tool'
    timestamp         DATETIME NOT NULL,
    archived          INTEGER NOT NULL DEFAULT 0,  -- compaction flag (week 4)
    FOREIGN KEY (investigation_id) REFERENCES investigations(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS messages_inv_seq_idx ON messages(investigation_id, seq);

CREATE TABLE IF NOT EXISTS tool_calls (
    id                TEXT PRIMARY KEY,      -- LLM-supplied call id
    investigation_id  TEXT NOT NULL,
    seq               INTEGER NOT NULL,
    tool              TEXT NOT NULL,
    input_json        TEXT NOT NULL,
    rationale         TEXT,
    status            TEXT NOT NULL,         -- pending | approved | edited | skipped | executed | failed | aborted
    decided_by        TEXT,
    task_id           TEXT,                  -- optional: hub.runner task created from this call
    created_at        DATETIME NOT NULL,
    decided_at        DATETIME,
    result_json       TEXT,                  -- summary/result fed back to LLM as 'tool' message
    FOREIGN KEY (investigation_id) REFERENCES investigations(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS tool_calls_inv_idx    ON tool_calls(investigation_id);
CREATE INDEX IF NOT EXISTS tool_calls_status_idx ON tool_calls(status);

CREATE TABLE IF NOT EXISTS findings (
    id                TEXT PRIMARY KEY,
    investigation_id  TEXT NOT NULL,
    severity          TEXT NOT NULL,         -- info | warn | error
    code              TEXT NOT NULL,
    message           TEXT NOT NULL,
    evidence_json     TEXT,                  -- JSON array of task_ids + extras
    pinned            INTEGER NOT NULL DEFAULT 0,
    ignored           INTEGER NOT NULL DEFAULT 0,
    created_at        DATETIME NOT NULL,
    FOREIGN KEY (investigation_id) REFERENCES investigations(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS findings_inv_idx ON findings(investigation_id);
