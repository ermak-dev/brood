package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// schemaModule — вклад модуля в общую схему (#205): DDL создания (таблицы,
// индексы, триггеры) и проверка того, что база текущей версии ему отвечает.
// Модуль ничего не знает о версиях: единственная версия объявлена в
// schemaVersion, читает её initializeSchema.
type schemaModule struct {
	id     string
	create func() string
	verify func(ctx context.Context, tx *sql.Tx) error
}

// schemaModules — единый список DDL базы. Порядок важен: внешние ключи
// требуют создания родительских таблиц раньше дочерних. Тест
// TestSchemaModulesCoverExpectedTables держит список таблиц полным.
var schemaModules = []schemaModule{
	{id: "core", create: func() string { return currentSchema }, verify: verifyCoreSchema},
	{id: "acl", create: aclSchema, verify: verifyACLSchema},
	{id: "project-capacity", create: func() string { return projectCapacitySchema }, verify: verifyProjectCapacitySchema},
	{id: "managed", create: managedFullSchema, verify: verifyManagedSchema},
	{id: "coordination", create: func() string { return coordinationSchema }, verify: verifyCoordinationSchema},
	{id: "project-secret", create: func() string { return projectSecretSchema }, verify: verifyProjectSecretSchema},
	{id: "origin", create: func() string { return projectRepositorySchema }, verify: verifyOriginSchema},
	{id: "git-credential", create: func() string { return projectGitCredentialSchema }, verify: verifyProjectGitCredentialSchema},
	{id: "pipelines", create: func() string { return swarmStepsSchema }, verify: verifySwarmStepsSchema},
	{id: "swarm-roles", create: func() string { return swarmRoleBindingSchema }, verify: verifySwarmRoleSchema},
	{id: "workflow", create: workflowFullSchema, verify: verifyWorkflowSchema},
	{id: "pipeline-source", create: pipelineSourceFullSchema, verify: verifyPipelineSourcesSchema},
	{id: "swarm-requests", create: swarmRequestsFullSchema, verify: verifySwarmRequestsSchema},
}

// aclSchema — ACL роёв: члены с ролями и уникальность имён неудалённых
// роёв. В чистой базе owner grant создаёт явная вставка владельца.
func aclSchema() string {
	return swarmMembersSchema + ";" + liveSwarmNameIndex + ";" +
		`INSERT INTO swarm_members (swarm_id, user_id, role) SELECT id, owner_id, 'owner' FROM swarms`
}

// workflowFullSchema — определения workflow и счётчики ревизий проектов.
func workflowFullSchema() string {
	return workflowDefinitionSchema + ";" + workflowRevisionSchema
}

// managedFullSchema — инвентарь Proxmox: пулы, акторы (включая stage,
// делегирование и origin), каталог подключений и ограничения уникальности.
func managedFullSchema() string {
	statements := []string{managedOriginSchema(), managedConnectionsRuntimeSchema()}
	for _, object := range managedConstraints {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

// pipelineSourceFullSchema — таблица источников запусков и триггеры
// неизменности источника (#158, #160). Идентичность запуска защищена
// неизменностью снимка графа роя: у роя один снимок (#206).
func pipelineSourceFullSchema() string {
	return pipelineSourceSchema + ";" + pipelineSourceImmutable + ";" +
		pipelineSourceNoReplace + ";" + pipelineSourceNoDelete + ";" +
		swarmGraphImmutable + ";" + swarmNoReplace
}

// swarmNoReplace запрещает подмену строки роя целиком (INSERT OR REPLACE):
// замена одной строки роя — это подмена прогона. Прямой дубликат ID
// и так нарушает первичный ключ; триггер закрывает именно REPLACE.
const swarmNoReplace = `CREATE TRIGGER swarms_no_replace BEFORE INSERT ON swarms WHEN EXISTS (SELECT 1 FROM swarms WHERE id=NEW.id) BEGIN SELECT RAISE(ABORT, 'swarm already exists'); END`

// swarmGraphImmutable делает снимок графа роя и личность определения
// неизменяемыми после привязки (#206): пока graph IS NULL, привязка
// возможна; после — любая правка или замена снимка и личности отказ.
// Это и защита идентичности запуска вместо бывшего
// pipeline_runs_no_replace: подменить строку роя — подменить прогон.
const swarmGraphImmutable = `CREATE TRIGGER swarm_graph_immutable BEFORE UPDATE ON swarms WHEN OLD.graph IS NOT NULL AND (NEW.graph IS NOT OLD.graph OR NEW.definition_name IS NOT OLD.definition_name OR NEW.definition_version IS NOT OLD.definition_version OR NEW.definition_revision IS NOT OLD.definition_revision) BEGIN SELECT RAISE(ABORT, 'swarm graph is immutable'); END`

// verifyCoreSchema сверяет базовые таблицы.
func verifyCoreSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, currentSchema)
}

// verifyACLSchema сверяет ACL и убеждается, что у каждого роя есть
// owner grant: повреждённая база не перестраивается по owner_id.
func verifyACLSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, swarmMembersSchema); err != nil {
		return err
	}
	if err := checkObject(ctx, tx, "index", "swarms_live_name", liveSwarmNameIndex); err != nil {
		return err
	}
	var orphaned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarms sw
		WHERE NOT EXISTS(SELECT 1 FROM swarm_members m
			WHERE m.swarm_id = sw.id AND m.user_id = sw.owner_id AND m.role = 'owner'))`).Scan(&orphaned); err != nil {
		return err
	}
	if orphaned {
		return fmt.Errorf("%w: swarm primary owner has no owner grant", errIncompatibleSchema)
	}
	return nil
}

// verifyProjectCapacitySchema сверяет лимиты проектов.
func verifyProjectCapacitySchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, projectCapacitySchema)
}

// verifyManagedSchema сверяет инвентарь и его ограничения уникальности.
func verifyManagedSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, managedOriginSchema()); err != nil {
		return err
	}
	if err := checkSchema(ctx, tx, managedConnectionsRuntimeSchema()); err != nil {
		return err
	}
	for _, object := range managedConstraints {
		if err := checkObject(ctx, tx, object.kind, object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}

// verifyCoordinationSchema сверяет таблицу координационных claims.
func verifyCoordinationSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, coordinationSchema)
}

// verifyProjectSecretSchema сверяет таблицы секретов проектов.
func verifyProjectSecretSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, projectSecretSchema)
}

// verifyOriginSchema сверяет repository проектов и origin акторов.
func verifyOriginSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, projectRepositorySchema); err != nil {
		return err
	}
	return checkSchema(ctx, tx, managedOriginSchema())
}

// verifySwarmStepsSchema сверяет шаги роя.
func verifySwarmStepsSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, swarmStepsSchema)
}

// verifySwarmRoleSchema сверяет привязки ролей роёв.
func verifySwarmRoleSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, swarmRoleBindingSchema)
}

// verifyWorkflowSchema сверяет определения и счётчики ревизий, включая
// согласованность счётчиков со строками определений.
func verifyWorkflowSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, workflowDefinitionSchema); err != nil {
		return err
	}
	if err := checkSchema(ctx, tx, workflowRevisionSchema); err != nil {
		return err
	}
	var invalid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_revision_counters WHERE typeof(version) != 'integer' OR version <= 0) OR EXISTS(SELECT 1 FROM workflow_definitions d LEFT JOIN workflow_revision_counters c ON c.project_id=d.project_id WHERE typeof(d.version) != 'integer' OR d.version <= 0 OR c.version IS NULL OR c.version < d.version OR d.revision IS NULL OR length(d.revision) != 32 OR d.revision GLOB '*[^0-9a-f]*')`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return fmt.Errorf("%w: workflow revision counters differ from definitions", errIncompatibleSchema)
	}
	return nil
}

// verifyPipelineSourcesSchema сверяет таблицу источников и триггеры
// неизменности.
func verifyPipelineSourcesSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, pipelineSourceSchema); err != nil {
		return err
	}
	for name, expected := range map[string]string{
		"pipeline_sources_immutable":  pipelineSourceImmutable,
		"pipeline_sources_no_replace": pipelineSourceNoReplace,
		"pipeline_sources_no_delete":  pipelineSourceNoDelete,
		"swarm_graph_immutable":       swarmGraphImmutable,
		"swarms_no_replace":           swarmNoReplace,
	} {
		if err := checkObject(ctx, tx, "trigger", name, expected); err != nil {
			return err
		}
	}
	return nil
}

// checkObject сверяет определение объекта sqlite_master заданного типа
// с ожидаемым; пробелы вне строковых литералов несущественны.
func checkObject(ctx context.Context, tx *sql.Tx, kind, name, expected string) error {
	var actual string
	err := tx.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`, kind, name).Scan(&actual)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: missing %s %s; recreate the database", errIncompatibleSchema, kind, name)
	}
	if err != nil {
		return fmt.Errorf("inspect %s %s: %w", kind, name, err)
	}
	if normalizeSchemaSQL(actual) != normalizeSchemaSQL(expected) {
		return fmt.Errorf("%w: %s %s differs from current schema; recreate the database", errIncompatibleSchema, kind, name)
	}
	return nil
}

// verifySwarmRequestsSchema сверяет таблицу запросов, её индексы и
// ограждение неизменности (#209).
func verifySwarmRequestsSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, swarmRequestsSchema); err != nil {
		return err
	}
	for _, object := range []struct{ kind, name, sql string }{
		{"index", "swarm_requests_open_node", swarmRequestOpenNode},
		{"index", "swarm_requests_step", swarmRequestStep},
		{"trigger", "swarm_requests_fence", swarmRequestFence},
		{"trigger", "swarm_requests_no_delete", swarmRequestNoDelete},
		{"trigger", "swarm_requests_no_replace", swarmRequestNoReplace},
		{"trigger", "swarm_requests_cancel_guard", swarmRequestCancelGuard},
		{"trigger", "swarms_gate_stop", swarmGateStop},
		{"trigger", "swarms_gate_resume", swarmGateResume},
	} {
		if err := checkObject(ctx, tx, object.kind, object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}
