-- Phase 2: server-side sessions, login bookkeeping and per-user project names.

-- Server-side sessions. The cookie carries a random token; only its SHA-256
-- hash is stored, so a leaked database cannot be used to hijack sessions.
CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash   TEXT    NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Synchronizer token for state-changing form submissions.
    csrf_token   TEXT    NOT NULL,
    -- One-shot message shown on the next rendered page (e.g. "Project created.").
    flash        TEXT    NOT NULL DEFAULT '',
    user_agent   TEXT    NOT NULL DEFAULT '',
    remote_addr  TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    last_seen_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Absolute expiry; idle expiry is computed from last_seen_at.
    expires_at   TEXT    NOT NULL
);
CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);

ALTER TABLE users ADD COLUMN last_login_at TEXT;

-- A user cannot have two projects with the same name (case-insensitive).
CREATE UNIQUE INDEX idx_projects_user_name ON projects(user_id, name COLLATE NOCASE);
