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
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    state      TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'deleted')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE swarm_ssh_keys (
    swarm_id    TEXT NOT NULL PRIMARY KEY REFERENCES swarms(id) ON DELETE CASCADE,
    public_key  TEXT NOT NULL UNIQUE,
    private_key TEXT NOT NULL
);

CREATE TABLE swarm_artifact_versions (
    id         TEXT NOT NULL PRIMARY KEY,
    swarm_id   TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    version    INTEGER NOT NULL CHECK (version > 0),
    media_type TEXT NOT NULL CHECK (media_type IN (
        'text/plain', 'text/html', 'image/png', 'image/jpeg', 'image/gif', 'image/webp'
    )),
    content    BLOB NOT NULL CHECK (typeof(content) = 'blob' AND length(content) > 0),
    UNIQUE (swarm_id, name, version)
);

CREATE TABLE telegram_artifact_replies (
    session_id TEXT NOT NULL REFERENCES sessions(token_hash) ON DELETE CASCADE,
    message_id INTEGER NOT NULL,
    swarm_id   TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    actor      TEXT NOT NULL,
    version_id TEXT NOT NULL REFERENCES swarm_artifact_versions(id) ON DELETE CASCADE,
    PRIMARY KEY (session_id, message_id)
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT,
    data       BLOB
, kind TEXT NOT NULL DEFAULT 'web' CHECK (kind IN ('web', 'telegram')));
