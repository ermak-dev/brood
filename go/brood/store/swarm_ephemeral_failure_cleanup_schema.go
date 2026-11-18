package store

import "strings"

// Исторические DDL 54/55 остаются неизменны. Новый kind хранится в том же
// provenance ledger; его вставка требует positive failure и применённый fail.
func ephemeralChildLifecycleV58(sql string) string {
	sql = strings.ReplaceAll(sql, "kind IN ('start','cleanup')", "kind IN ('start','cleanup','failure_cleanup')")
	sql = strings.ReplaceAll(sql, "(intent_id=NEW.intent_id AND kind=NEW.kind)", "(intent_id=NEW.intent_id AND (kind=NEW.kind OR (kind IN ('cleanup','failure_cleanup') AND NEW.kind IN ('cleanup','failure_cleanup'))))")
	return strings.ReplaceAll(sql,
		"EXISTS(SELECT 1 FROM ephemeral_child_result_seals r WHERE r.intent_id=a.intent_id)",
		`EXISTS(SELECT 1 FROM ephemeral_child_result_seals r WHERE r.intent_id=a.intent_id))
 OR (NEW.kind='failure_cleanup' AND m.operation='delete' AND m.stage='stop' AND m.phase='deleting' AND m.desired='deleted' AND m.error='stop not requested'
 AND EXISTS(SELECT 1 FROM swarm_steps v JOIN swarm_child_joins j ON j.swarm_id=v.swarm_id AND j.parent_sequence=v.sequence
 WHERE v.swarm_id=i.swarm_id AND v.sequence=i.parent_sequence AND j.pass=i.pass AND v.actor_uid=i.parent_uid AND v.actor_incarnation=i.parent_incarnation AND v.request_id=i.parent_request_id
 AND v.state='completed' AND v.outcome='fail' AND v.attested_commit=i.source_commit AND j.state='open'
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions x WHERE x.swarm_id=i.swarm_id AND x.sequence=i.parent_sequence))
 AND (EXISTS(SELECT 1 FROM ephemeral_child_failures f WHERE f.intent_id=a.intent_id) OR EXISTS(SELECT 1 FROM ephemeral_child_executor_failures f WHERE f.intent_id=a.intent_id))`)
}

func ephemeralFailedLifecycleV58(sql string) string {
	return strings.ReplaceAll(sql, "ON ephemeral_child_lifecycles WHEN EXISTS(SELECT 1 FROM ephemeral_child_failures", "ON ephemeral_child_lifecycles WHEN NEW.kind!='failure_cleanup' AND EXISTS(SELECT 1 FROM ephemeral_child_failures")
}

func ephemeralChildFailureCleanupMigration() string {
	table := ephemeralChildLifecycleV58(ephemeralChildLifecycleTable)
	statements := []string{
		`CREATE TEMP TABLE ephemeral_child_lifecycles_backup AS SELECT * FROM ephemeral_child_lifecycles`,
		`DROP TABLE ephemeral_child_lifecycles`,
		table,
		`INSERT INTO ephemeral_child_lifecycles SELECT * FROM ephemeral_child_lifecycles_backup`,
		`DROP TABLE ephemeral_child_lifecycles_backup`,
	}
	for _, o := range ephemeralChildLifecycleObjects {
		statements = append(statements, ephemeralChildLifecycleV58(o.sql))
	}
	for _, o := range ephemeralChildFailureObjects {
		if o.name == "ephemeral_failed_lifecycle_guard" {
			statements = append(statements, ephemeralFailedLifecycleV58(o.sql))
		}
	}
	return strings.Join(statements, ";")
}
