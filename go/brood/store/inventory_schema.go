package store

import _ "embed"

// inventorySchema — инвентарь Proxmox: пулы, агенты и каталог подключений.
// Таблицы managed_pools и managed_connections сохраняют имена и колонки:
// каждая выкладка применяет фикстуру, выгруженную с ними.
// Секрет токена подключения хранится рядом с его полями, но читается
// только кодом создания PVE-клиента.
//
//go:embed inventory.sql
var inventorySchema string

var inventoryConstraints = []struct{ name, kind, sql string }{
	{"managed_pools_live_name", "index", `CREATE UNIQUE INDEX managed_pools_live_name ON managed_pools(name) WHERE state <> 'deleted'`},
	{"agents_live_name", "index", `CREATE UNIQUE INDEX agents_live_name ON agents(swarm_id, name) WHERE phase <> 'deleted'`},
	{"agents_live_ctid", "index", `CREATE UNIQUE INDEX agents_live_ctid ON agents(cluster_identity, ctid) WHERE phase <> 'deleted'`},
	{"agents_live_role", "index", `CREATE INDEX agents_live_role ON agents(live_role_id, name, uid) WHERE live_role_id IS NOT NULL`},
	{"swarm_delete_guard", "trigger", `CREATE TRIGGER swarm_delete_guard BEFORE DELETE ON swarms WHEN EXISTS (SELECT 1 FROM agents WHERE swarm_id = OLD.id AND phase <> 'deleted') BEGIN SELECT RAISE(ABORT, 'swarm has agents'); END`},
}
