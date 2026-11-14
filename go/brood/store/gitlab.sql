CREATE TABLE gitlab_bindings (
    project_id TEXT PRIMARY KEY NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repository TEXT NOT NULL,
    api_origin TEXT NOT NULL,
    gitlab_project_id INTEGER NOT NULL CHECK (gitlab_project_id > 0),
    target_branch TEXT NOT NULL CHECK (length(target_branch) > 0),
    credential_ref TEXT NOT NULL CHECK (length(credential_ref) > 0),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    revision INTEGER NOT NULL CHECK (revision > 0)
);
CREATE TABLE gitlab_grants (
    id TEXT PRIMARY KEY NOT NULL,
    project_id TEXT NOT NULL,
    swarm_id TEXT NOT NULL,
    atespace_uid TEXT NOT NULL,
    actor_uid TEXT NOT NULL,
    incarnation INTEGER NOT NULL CHECK (incarnation > 0),
    human_id TEXT NOT NULL,
    binding_revision INTEGER NOT NULL CHECK (binding_revision > 0),
    can_read INTEGER NOT NULL CHECK (can_read IN (0, 1)),
    can_create INTEGER NOT NULL CHECK (can_create IN (0, 1)),
    can_propose INTEGER NOT NULL CHECK (can_propose IN (0, 1)),
    expires_at INTEGER NOT NULL,
    revoked INTEGER NOT NULL DEFAULT 0 CHECK (revoked IN (0, 1)),
    revision INTEGER NOT NULL CHECK (revision > 0)
);
CREATE TABLE gitlab_operations (
    id TEXT PRIMARY KEY NOT NULL,
    project_id TEXT NOT NULL,
    swarm_id TEXT NOT NULL,
    atespace_uid TEXT NOT NULL,
    actor_uid TEXT NOT NULL,
    incarnation INTEGER NOT NULL CHECK (incarnation > 0),
    kind TEXT NOT NULL CHECK (kind IN ('create', 'propose')),
    request_id TEXT NOT NULL CHECK (length(request_id) > 0),
    grant_id TEXT NOT NULL REFERENCES gitlab_grants(id),
    binding_revision INTEGER NOT NULL CHECK (binding_revision > 0),
    binding_snapshot TEXT NOT NULL CHECK (json_valid(binding_snapshot)),
    payload_hash TEXT NOT NULL CHECK (length(payload_hash) = 64 AND payload_hash NOT GLOB '*[^0-9a-f]*'),
    snapshot TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reserved', 'sending', 'completed', 'refused', 'unknown')),
    revision INTEGER NOT NULL CHECK (revision > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (project_id, actor_uid, incarnation, kind, request_id)
);
CREATE TABLE gitlab_approvals (
    operation_id TEXT PRIMARY KEY NOT NULL REFERENCES gitlab_operations(id),
    human_id TEXT NOT NULL,
    session_hash TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('approved', 'revoked', 'claimed')),
    revision INTEGER NOT NULL CHECK (revision > 0)
);
