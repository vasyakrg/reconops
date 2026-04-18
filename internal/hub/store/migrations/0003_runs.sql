CREATE TABLE IF NOT EXISTS runs (
    id                TEXT PRIMARY KEY,
    investigation_id  TEXT,                  -- null for manual runs (week 2)
    name              TEXT,
    selector_json     TEXT,
    created_by        TEXT,
    created_at        DATETIME NOT NULL,
    finished_at       DATETIME,
    status            TEXT NOT NULL          -- pending|running|done|aborted
);

CREATE INDEX IF NOT EXISTS runs_created_at_idx ON runs(created_at);

CREATE TABLE IF NOT EXISTS tasks (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL,
    host_id       TEXT NOT NULL,
    collector     TEXT NOT NULL,
    params_json   TEXT,
    status        TEXT NOT NULL,             -- pending|sent|ok|error|timeout|canceled|undeliverable
    started_at    DATETIME,
    finished_at   DATETIME,
    duration_ms   INTEGER,
    error         TEXT,
    FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS tasks_run_idx ON tasks(run_id);
CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks(status);

CREATE TABLE IF NOT EXISTS results (
    task_id       TEXT PRIMARY KEY,
    data_json     TEXT,
    hints_json    TEXT,
    stderr        TEXT,
    exit_code     INTEGER,
    artifact_dir  TEXT,
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
