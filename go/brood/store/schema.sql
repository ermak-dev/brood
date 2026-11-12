CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT,
    password_hash TEXT NOT NULL,
    is_admin      INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE project_members (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (project_id, user_id)
);

CREATE TABLE swarms (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    owner_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    atespace_name TEXT NOT NULL UNIQUE,
    atespace_uid  TEXT,
    state         TEXT NOT NULL DEFAULT 'provisioning',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    retry         TEXT NOT NULL DEFAULT '' CHECK (retry IN ('', 'create', 'delete')),
    graph         BLOB,
    execution     TEXT NOT NULL DEFAULT 'idle' CHECK (execution IN ('idle', 'running', 'waiting', 'succeeded', 'failed', 'stopped')),
    definition_name     TEXT,
    definition_version  INTEGER,
    definition_revision TEXT
, execution_at TEXT);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT,
    data       BLOB
);
