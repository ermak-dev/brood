CREATE TABLE project_secret_revisions (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (revision >= 0)
);
CREATE TABLE project_secret_files (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    data BLOB NOT NULL CHECK (length(data) <= 524288),
    PRIMARY KEY (project_id, name)
);
CREATE TABLE project_secret_vars (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    value TEXT NOT NULL CHECK (length(CAST(value AS BLOB)) <= 16384),
    PRIMARY KEY (project_id, name)
);
