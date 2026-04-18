-- Persistent operator sessions. Survives hub restarts so a `compose up -d`
-- doesn't kick everyone back to the login page.
--
-- Login throttle counters and flash messages stay in memory (per-process,
-- short-lived, no value in surviving restarts).

CREATE TABLE IF NOT EXISTS web_sessions (
    sid         TEXT PRIMARY KEY,           -- 32-byte hex, opaque to clients
    username    TEXT NOT NULL,
    csrf_token  TEXT NOT NULL,              -- double-submit CSRF token
    created_at  DATETIME NOT NULL,
    expires_at  DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS web_sessions_expires_idx ON web_sessions(expires_at);
