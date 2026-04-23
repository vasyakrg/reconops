-- API tokens for programmatic access to /api/v1/*.
-- Separate from bootstrap_tokens (agent enrollment) and web_sessions
-- (operator browser auth). Operator issues PATs from /settings/api-tokens;
-- callers present them as Authorization: Bearer recon_pat_<raw>.

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,        -- ULID
    name         TEXT NOT NULL,           -- operator-supplied label
    token_hash   TEXT NOT NULL,           -- hex sha256(raw token)
    prefix       TEXT NOT NULL,           -- first 12 chars of raw token for UI (incl. "recon_pat_")
    scope        TEXT NOT NULL,           -- 'read' | 'investigate' | 'admin'
    created_by   TEXT NOT NULL,           -- operator username (or 'bootstrap')
    created_at   DATETIME NOT NULL,
    last_used_at DATETIME,
    expires_at   DATETIME,                -- NULL = no expiry
    revoked_at   DATETIME,
    CHECK (scope IN ('read','investigate','admin'))
);

CREATE UNIQUE INDEX idx_api_tokens_hash ON api_tokens(token_hash);
CREATE INDEX idx_api_tokens_created_at ON api_tokens(created_at DESC);
