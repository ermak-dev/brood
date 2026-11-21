CREATE TABLE coordination_claims (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind = 'workspace'),
    request_id TEXT NOT NULL CHECK (request_id <> ''),
    payload TEXT NOT NULL,
    slot TEXT,
    PRIMARY KEY (project_id, kind, request_id),
    UNIQUE (project_id, kind, slot)
);
CREATE TABLE command_project_details (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    description TEXT NOT NULL DEFAULT ''
);
