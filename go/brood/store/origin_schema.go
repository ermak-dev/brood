package store

import "strings"

// managedOriginColumn — канонический HTTPS origin актора (#146); пустой —
// пустой workspace.
const managedOriginColumn = `origin TEXT NOT NULL DEFAULT ''`

// projectRepositorySchema — repository проекта: один из remotes установки,
// выбранный администратором. Отсутствие строки — repository не задан.
const projectRepositorySchema = `CREATE TABLE project_repositories (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    origin TEXT NOT NULL CHECK (origin <> '' AND length(CAST(origin AS BLOB)) <= 1024)
)`

func managedOriginSchema() string {
	return strings.TrimSuffix(strings.TrimSpace(managedDelegationSchema()), ");") + "," + managedOriginColumn + ");"
}

// Регистрация origin в общей схеме — schemaModules ("origin") и
// verifyOriginSchema в registry.go (#205). Модуль не знает о версиях.
