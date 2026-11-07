package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// managedOriginColumn — канонический HTTPS origin актора (#146); пустой —
// пустой workspace. Схема 10 добавляет его к прежним строкам без сброса.
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

// initializeOrigins расширяет только распознанную схему 9 и проверяет 10.
func initializeOrigins(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 9 {
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='project_repositories'`).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return errors.New("incompatible database schema: unversioned project repository objects")
		}
		for _, statement := range []string{
			`ALTER TABLE managed_actors ADD COLUMN ` + managedOriginColumn,
			projectRepositorySchema,
			`PRAGMA user_version=10`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return errors.New("cannot extend actor origin schema")
			}
		}
	} else if version != 10 && version != 11 && version != 12 && version != 13 && version != 14 {
		return errors.New("incompatible database schema: unsupported actor origin version")
	}
	if err := checkSchema(ctx, tx, projectRepositorySchema); err != nil {
		return err
	}
	return checkSchema(ctx, tx, managedOriginSchema())
}
