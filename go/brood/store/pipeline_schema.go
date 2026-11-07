package store

import (
	"context"
	"database/sql"
	"errors"
)

// pipelineSchema хранит снимок графа и отдельную строку для каждого посещения.
// Доступ проверяет API: администратору не обязательно состоять в рое.
// Удаление владельца каскадно удаляет его запуски, как и принадлежащие ему рои.
const pipelineSchema = `CREATE TABLE pipeline_runs (
    id TEXT NOT NULL PRIMARY KEY,
    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    graph BLOB NOT NULL,
    status TEXT NOT NULL
);
CREATE TABLE pipeline_steps (
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL,
    node_id TEXT NOT NULL,
    state TEXT NOT NULL,
    answer TEXT,
    outcome TEXT,
    actor_uid TEXT,
    actor_incarnation INTEGER,
    request_id TEXT,
    native_actor_name TEXT,
    native_template_name TEXT,
    native_template_uid TEXT,
    native_template_digest TEXT,
    PRIMARY KEY (run_id, sequence),
    UNIQUE (run_id, request_id)
)`

func initializePipelines(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 10 {
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name IN ('pipeline_runs', 'pipeline_steps')`).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return errors.New("incompatible database schema: unversioned pipeline objects")
		}
		if _, err := tx.ExecContext(ctx, pipelineSchema+`; PRAGMA user_version=11`); err != nil {
			return errors.New("cannot extend pipeline schema")
		}
	} else if version != 11 && version != 12 && version != 13 {
		return errors.New("incompatible database schema: unsupported pipeline version")
	}
	return checkSchema(ctx, tx, pipelineSchema)
}
