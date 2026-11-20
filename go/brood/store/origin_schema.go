package store

// projectRepositorySchema — repository проекта: один из remotes установки,
// выбранный администратором. Отсутствие строки — repository не задан.
const projectRepositorySchema = `CREATE TABLE project_repositories (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    origin TEXT NOT NULL CHECK (origin <> '' AND length(CAST(origin AS BLOB)) <= 1024)
)`
