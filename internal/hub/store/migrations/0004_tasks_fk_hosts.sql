-- (C3) tasks.host_id should reference hosts(id) so revoke + cleanup cascades
-- to abandoned tasks. SQLite has no ALTER TABLE ADD CONSTRAINT — recreate.
PRAGMA foreign_keys = OFF;

CREATE TABLE tasks_new (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL,
    host_id       TEXT NOT NULL,
    collector     TEXT NOT NULL,
    params_json   TEXT,
    status        TEXT NOT NULL,
    started_at    DATETIME,
    finished_at   DATETIME,
    duration_ms   INTEGER,
    error         TEXT,
    FOREIGN KEY (run_id)  REFERENCES runs(id)  ON DELETE CASCADE,
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE
);

INSERT INTO tasks_new SELECT * FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;

CREATE INDEX IF NOT EXISTS tasks_run_idx    ON tasks(run_id);
CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks(status);

PRAGMA foreign_keys = ON;
