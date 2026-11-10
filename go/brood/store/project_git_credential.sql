CREATE TABLE project_git_credentials (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 63),
    repository TEXT NOT NULL CHECK (repository <> '' AND length(CAST(repository AS BLOB)) <= 1024),
    username TEXT NOT NULL CHECK (username <> '' AND length(CAST(username AS BLOB)) <= 256),
    token TEXT NOT NULL CHECK (token <> '' AND length(CAST(token AS BLOB)) <= 4096),
    PRIMARY KEY (project_id, name)
);
CREATE TABLE project_git_credential_refs (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 63)
)
