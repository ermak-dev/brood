package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
)

//go:embed project_secret.sql
var projectSecretSchema string

func initializeProjectSecrets(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 8 {
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name GLOB 'project_secret_*'`).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return errors.New("incompatible database schema: unversioned project secret objects")
		}
		if _, err := tx.ExecContext(ctx, projectSecretSchema+`PRAGMA user_version=9`); err != nil {
			return errors.New("cannot extend project secret schema")
		}
	} else if version != 9 && version != 10 && version != 11 && version != 12 && version != 13 {
		return errors.New("incompatible database schema: unsupported project secret version")
	}
	return checkSchema(ctx, tx, projectSecretSchema)
}
