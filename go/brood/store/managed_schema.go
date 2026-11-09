package store

import (
	_ "embed"
	"strings"
)

//go:embed managed.sql
var managedSchema string

// stage — стадия жизненного цикла задачи актора; managed.sql хранит
// историческое определение схемы 3, текущая таблица включает колонку stage.
const managedStageColumn = `stage TEXT NOT NULL DEFAULT 'reconcile' CHECK (stage IN ('reconcile', 'create', 'start', 'stop', 'delete', 'done'))`

func managedLifecycleSchema() string {
	return strings.TrimSuffix(strings.TrimSpace(managedSchema), ");") + "," + managedStageColumn + ");"
}

var managedConstraints = []struct{ name, kind, sql string }{
	{"managed_pools_live_name", "index", `CREATE UNIQUE INDEX managed_pools_live_name ON managed_pools(name) WHERE state <> 'deleted'`},
	{"managed_actors_live_name", "index", `CREATE UNIQUE INDEX managed_actors_live_name ON managed_actors(swarm_id, name) WHERE phase <> 'deleted'`},
	{"managed_actors_live_ctid", "index", `CREATE UNIQUE INDEX managed_actors_live_ctid ON managed_actors(cluster_identity, ctid) WHERE phase <> 'deleted'`},
	{"managed_swarm_delete_guard", "trigger", `CREATE TRIGGER managed_swarm_delete_guard BEFORE DELETE ON swarms WHEN EXISTS (SELECT 1 FROM managed_actors WHERE swarm_id = OLD.id AND phase <> 'deleted') BEGIN SELECT RAISE(ABORT, 'swarm has managed actors'); END`},
}

var managedDelegationColumns = []string{
	`role TEXT NOT NULL DEFAULT 'worker' CHECK (role IN ('worker', 'master'))`,
	`delegator_id TEXT NOT NULL DEFAULT ''`,
	`parent_uid TEXT NOT NULL DEFAULT ''`,
}

func managedDelegationSchema() string {
	return strings.TrimSuffix(strings.TrimSpace(managedLifecycleSchema()), ");") + "," + strings.Join(managedDelegationColumns, ",") + ");"
}

func managedConnectionsRuntimeSchema() string {
	return strings.TrimSuffix(strings.TrimSpace(managedConnectionsSchema), ")") + ",runtime_config TEXT NOT NULL DEFAULT '')"
}

// managedConnectionsSchema — каталог подключений Proxmox панели (#118).
// Секрет хранится рядом с полями подключения, но читается только кодом
// создания PVE-клиента.
const managedConnectionsSchema = `CREATE TABLE managed_connections (
	uid TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	endpoint TEXT NOT NULL,
	token_id TEXT NOT NULL,
	token_secret TEXT NOT NULL,
	ca_pem TEXT NOT NULL,
	node TEXT NOT NULL,
	resource_pool TEXT NOT NULL,
	storage TEXT NOT NULL,
	network TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`

// Регистрация инвентаря в общей схеме — managedFullSchema и
// verifyManagedSchema в registry.go (#205). Модуль не знает о версиях.
