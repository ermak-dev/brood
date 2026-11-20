package store

import _ "embed"

// managedSchema — инвентарь Proxmox: пулы, воркеры и каталог подключений.
// Секрет токена подключения хранится рядом с его полями, но читается
// только кодом создания PVE-клиента.
//
//go:embed managed.sql
var managedSchema string

var managedConstraints = []struct{ name, kind, sql string }{
	{"managed_pools_live_name", "index", `CREATE UNIQUE INDEX managed_pools_live_name ON managed_pools(name) WHERE state <> 'deleted'`},
	{"managed_actors_live_name", "index", `CREATE UNIQUE INDEX managed_actors_live_name ON managed_actors(swarm_id, name) WHERE phase <> 'deleted'`},
	{"managed_actors_live_ctid", "index", `CREATE UNIQUE INDEX managed_actors_live_ctid ON managed_actors(cluster_identity, ctid) WHERE phase <> 'deleted'`},
	{"managed_swarm_delete_guard", "trigger", `CREATE TRIGGER managed_swarm_delete_guard BEFORE DELETE ON swarms WHEN EXISTS (SELECT 1 FROM managed_actors WHERE swarm_id = OLD.id AND phase <> 'deleted') BEGIN SELECT RAISE(ABORT, 'swarm has managed actors'); END`},
}
