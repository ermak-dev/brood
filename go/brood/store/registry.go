package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// schemaModule — вклад модуля в общую схему: DDL создания (таблицы,
// индексы, триггеры) и проверка того, что база ему отвечает. Миграций
// нет: база другой версии отвергается и пересоздаётся оператором.
type schemaModule struct {
	id     string
	create func() string
	verify func(ctx context.Context, tx *sql.Tx) error
}

// schemaModules — единый список DDL базы. Порядок важен: внешние ключи
// требуют создания родительских таблиц раньше дочерних.
var schemaModules = []schemaModule{
	{id: "core", create: func() string { return currentSchema + ";" + liveSwarmNameIndex }, verify: verifyCoreSchema},
	{id: "project-capacity", create: func() string { return projectCapacitySchema }, verify: verifyProjectCapacitySchema},
	{id: "agent-roles", create: func() string { return agentRoleSchema + ";" + agentRoleDefaultIndex }, verify: verifyAgentRoleSchema},
	{id: "managed", create: managedFullSchema, verify: verifyManagedSchema},
	{id: "coordination", create: func() string { return coordinationSchema }, verify: verifyCoordinationSchema},
	{id: "project-secret", create: func() string { return projectSecretSchema }, verify: verifyProjectSecretSchema},
	{id: "origin", create: func() string { return projectRepositorySchema }, verify: verifyOriginSchema},
	{id: "git-credential", create: func() string { return projectGitCredentialSchema }, verify: verifyProjectGitCredentialSchema},
	{id: "telegram", create: telegramFullSchema, verify: verifyTelegramSchema},
	{id: "project-ownership", create: func() string { return projectOwnershipSchema }, verify: verifyProjectOwnershipSchema},
	{id: "project-images", create: func() string { return projectImageSchema }, verify: verifyProjectImageSchema},
}

// liveSwarmNameIndex: имя неудалённого роя уникально внутри проекта.
const liveSwarmNameIndex = `CREATE UNIQUE INDEX swarms_live_name ON swarms(project_id, name) WHERE state <> 'deleted'`

// prepareSchema создаёт пустую базу по реестру или сверяет базу версии
// want с ним. Любая другая версия — отказ: миграций нет.
func prepareSchema(ctx context.Context, conn *sql.Conn, want int) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer tx.Rollback()
	var version, objects int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("inspect schema version: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&objects); err != nil {
		return fmt.Errorf("inspect database schema: %w", err)
	}
	switch {
	case version == 0 && objects == 0:
		for _, module := range schemaModules {
			if _, err := tx.ExecContext(ctx, module.create()); err != nil {
				return fmt.Errorf("initialize database schema (%s): %w", module.id, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, want)); err != nil {
			return fmt.Errorf("initialize schema version: %w", err)
		}
	case version == want:
		for _, module := range schemaModules {
			if err := module.verify(ctx, tx); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: found version %d, want %d; there are no migrations, recreate the database",
			errIncompatibleSchema, version, want)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema initialization: %w", err)
	}
	return nil
}

// managedFullSchema — инвентарь Proxmox и его ограничения уникальности.
func managedFullSchema() string {
	statements := []string{managedSchema}
	for _, object := range managedConstraints {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

// verifyCoreSchema сверяет базовые таблицы и индекс имён роёв.
func verifyCoreSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, currentSchema); err != nil {
		return err
	}
	return checkObject(ctx, tx, "index", "swarms_live_name", liveSwarmNameIndex)
}

// verifyProjectCapacitySchema сверяет лимиты проектов.
func verifyProjectCapacitySchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, projectCapacitySchema)
}

// verifyManagedSchema сверяет инвентарь и его ограничения уникальности.
func verifyManagedSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, managedSchema); err != nil {
		return err
	}
	for _, object := range managedConstraints {
		if err := checkObject(ctx, tx, object.kind, object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}

// verifyCoordinationSchema сверяет таблицу броней операций над workspace.
func verifyCoordinationSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, coordinationSchema)
}

// verifyProjectSecretSchema сверяет таблицы секретов проектов.
func verifyProjectSecretSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, projectSecretSchema)
}

// verifyOriginSchema сверяет repository проектов.
func verifyOriginSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, projectRepositorySchema)
}

// checkObject сверяет определение объекта sqlite_master заданного типа
// с ожидаемым; пробелы вне строковых литералов несущественны.
func checkObject(ctx context.Context, tx *sql.Tx, kind, name, expected string) error {
	var actual string
	err := tx.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`, kind, name).Scan(&actual)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: missing %s %s", errIncompatibleSchema, kind, name)
	}
	if err != nil {
		return fmt.Errorf("inspect %s %s: %w", kind, name, err)
	}
	if normalizeSchemaSQL(actual) != normalizeSchemaSQL(expected) {
		return fmt.Errorf("%w: %s %s differs from current schema", errIncompatibleSchema, kind, name)
	}
	return nil
}
