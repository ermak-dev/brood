package store

import (
	_ "embed"
)

//go:embed swarm_members.sql
var swarmMembersSchema string

const liveSwarmNameIndex = `CREATE UNIQUE INDEX swarms_live_name ON swarms(project_id, name) WHERE state <> 'deleted'`

// Регистрация ACL в общей схеме — aclSchema и verifyACLSchema
// в registry.go (#205). Модуль не знает о версиях схемы.
