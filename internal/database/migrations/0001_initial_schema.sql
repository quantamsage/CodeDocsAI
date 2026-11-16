-- Initial schema for the AI code documentation platform.
--
-- Conventions:
--   * Timestamps are stored as UTC text in RFC 3339 format (strftime below).
--   * Every user-owned row can be traced back to users.id so that
--     authorization checks can always be expressed as a join on user_id.
--   * Child rows cascade on delete so deleting a project removes all of its data.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    email         TEXT    NOT NULL COLLATE NOCASE UNIQUE,
    display_name  TEXT    NOT NULL DEFAULT '',
    password_hash TEXT    NOT NULL,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE projects (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id              INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name                 TEXT    NOT NULL,
    description          TEXT    NOT NULL DEFAULT '',
    -- Primary language detected from uploaded files; languages_json holds the full breakdown.
    primary_language     TEXT    NOT NULL DEFAULT '',
    languages_json       TEXT    NOT NULL DEFAULT '{}',
    -- Lifecycle of the project's content: empty -> uploaded -> analyzing -> analyzed -> documented / failed.
    status               TEXT    NOT NULL DEFAULT 'empty'
                         CHECK (status IN ('empty', 'uploaded', 'analyzing', 'analyzed', 'documenting', 'documented', 'failed')),
    file_count           INTEGER NOT NULL DEFAULT 0,
    total_size_bytes     INTEGER NOT NULL DEFAULT 0,
    uploaded_at          TEXT,
    documentation_updated_at TEXT,
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX idx_projects_user_id ON projects(user_id);

CREATE TABLE project_files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Slash-separated path relative to the project root, as uploaded.
    path         TEXT    NOT NULL,
    language     TEXT    NOT NULL DEFAULT '',
    kind         TEXT    NOT NULL DEFAULT 'other'
                 CHECK (kind IN ('source', 'config', 'manifest', 'documentation', 'other')),
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    sha256       TEXT    NOT NULL DEFAULT '',
    -- Location of the stored content relative to the application data directory.
    storage_path TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (project_id, path)
);
CREATE INDEX idx_project_files_project_id ON project_files(project_id);

CREATE TABLE jobs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id       INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    -- e.g. 'analyze_project', 'generate_documentation', 'regenerate_file_docs'.
    type             TEXT    NOT NULL,
    status           TEXT    NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled')),
    progress_current INTEGER NOT NULL DEFAULT 0,
    progress_total   INTEGER NOT NULL DEFAULT 0,
    progress_message TEXT    NOT NULL DEFAULT '',
    attempts         INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL DEFAULT 3,
    payload_json     TEXT    NOT NULL DEFAULT '{}',
    error            TEXT    NOT NULL DEFAULT '',
    -- Earliest time the job may be picked up (used for retry back-off).
    run_after        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    started_at       TEXT,
    finished_at      TEXT,
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX idx_jobs_status_run_after ON jobs(status, run_after);
CREATE INDEX idx_jobs_project_id ON jobs(project_id);

CREATE TABLE analysis_results (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- NULL for batch- and project-level results.
    file_id       INTEGER REFERENCES project_files(id) ON DELETE CASCADE,
    job_id        INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
    -- 'structure' = parser-extracted facts, 'file'/'batch' = AI per-file/group analysis,
    -- 'project' = synthesized project-level understanding.
    kind          TEXT    NOT NULL CHECK (kind IN ('structure', 'file', 'batch', 'project')),
    result_json   TEXT    NOT NULL DEFAULT '{}',
    model         TEXT    NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX idx_analysis_results_project_kind ON analysis_results(project_id, kind);
CREATE INDEX idx_analysis_results_file_id ON analysis_results(file_id);

CREATE TABLE documentation (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Set when the document describes a single file.
    file_id     INTEGER REFERENCES project_files(id) ON DELETE SET NULL,
    job_id      INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
    -- 'project' = whole-project section, 'module' = directory/package, 'file' = single file.
    scope       TEXT    NOT NULL CHECK (scope IN ('project', 'module', 'file')),
    -- Section key (e.g. 'overview', 'architecture') or module/file path for narrower scopes.
    scope_key   TEXT    NOT NULL,
    title       TEXT    NOT NULL DEFAULT '',
    content_md  TEXT    NOT NULL DEFAULT '',
    -- Monotonic version per (project_id, scope, scope_key); the highest is current.
    version     INTEGER NOT NULL DEFAULT 1,
    model       TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (project_id, scope, scope_key, version)
);
CREATE INDEX idx_documentation_project_id ON documentation(project_id);
