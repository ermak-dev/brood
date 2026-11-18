package store

import "strings"

// Генераторы 54/58/60 неизменны. Новая provenance не переименовывает историю.
func ephemeralChildLifecycleV61(ddl string) string {
	ddl = ephemeralChildLifecycleV60(ddl)
	ddl = strings.ReplaceAll(ddl, "kind IN ('start','cleanup','failure_cleanup')", "kind IN ('start','cleanup','failure_cleanup','cancel_cleanup')")
	ddl = strings.ReplaceAll(ddl, "kind IN ('cleanup','failure_cleanup')", "kind IN ('cleanup','failure_cleanup','cancel_cleanup')")
	return strings.ReplaceAll(ddl, "OR (NEW.kind='failure_cleanup'", `OR (NEW.kind='cancel_cleanup' AND m.operation='delete' AND m.stage='stop' AND m.phase='deleting' AND m.desired='deleted' AND m.error='stop not requested'
 AND EXISTS(SELECT 1 FROM swarm_child_tasks c JOIN ephemeral_child_attestations p ON p.intent_id=i.id
 WHERE c.swarm_id=i.swarm_id AND c.parent_sequence=i.parent_sequence AND c.request_id=i.request_id
 AND c.actor_uid=a.actor_uid AND c.actor_incarnation=a.actor_incarnation AND c.payload=i.payload
 AND c.state='cancelled' AND c.revision=2 AND c.finished_at IS NOT NULL AND c.finished_at<>'' AND c.created_at<>''
 AND c.outcome IS NULL AND c.report IS NULL AND c.role='' AND c.delivery='' AND c.delivery_detail=''
 AND p.state='attested' AND p.actor_uid=a.actor_uid AND p.actor_incarnation=a.actor_incarnation
 AND p.origin=i.origin AND p.source_commit=i.source_commit AND p.attested_origin=i.origin AND p.attested_commit=i.source_commit
 AND NOT EXISTS(SELECT 1 FROM actor_deliveries d WHERE d.source_kind='child' AND d.source_id=c.id))
 AND NOT EXISTS(SELECT 1 FROM ephemeral_child_result_seals r WHERE r.intent_id=i.id)
 AND NOT EXISTS(SELECT 1 FROM ephemeral_child_failures f WHERE f.intent_id=i.id)
 AND NOT EXISTS(SELECT 1 FROM ephemeral_child_executor_failures f WHERE f.intent_id=i.id)
 AND EXISTS(SELECT 1 FROM swarm_steps v JOIN swarm_child_joins j ON j.swarm_id=v.swarm_id AND j.parent_sequence=v.sequence
 WHERE v.swarm_id=i.swarm_id AND v.sequence=i.parent_sequence AND j.pass=i.pass
 AND j.project_id=i.project_id AND j.atespace_uid=i.atespace_uid AND j.parent_uid=i.parent_uid AND j.parent_incarnation=i.parent_incarnation AND j.parent_request_id=i.parent_request_id
 AND v.actor_uid=i.parent_uid AND v.actor_incarnation=i.parent_incarnation AND v.request_id=i.parent_request_id
 AND v.state='completed' AND v.outcome IN ('success','fail') AND v.attested_commit=i.source_commit AND j.state='open'
 AND NOT (`+swarmChildUnfinished+`)
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions x WHERE x.swarm_id=i.swarm_id AND x.sequence=i.parent_sequence)))
 OR (NEW.kind='failure_cleanup'`)
}

func ephemeralChildCancelCleanupMigration() string {
	statements := []string{
		`CREATE TEMP TABLE ephemeral_child_lifecycles_backup AS SELECT * FROM ephemeral_child_lifecycles`,
		`DROP TABLE ephemeral_child_lifecycles`,
		ephemeralChildLifecycleV61(ephemeralChildLifecycleTable),
		`INSERT INTO ephemeral_child_lifecycles SELECT * FROM ephemeral_child_lifecycles_backup`,
		`DROP TABLE ephemeral_child_lifecycles_backup`,
	}
	for _, object := range ephemeralChildLifecycleObjects {
		statements = append(statements, ephemeralChildLifecycleV61(object.sql))
	}
	for _, object := range ephemeralChildFailureObjects {
		if object.name == "ephemeral_failed_lifecycle_guard" {
			statements = append(statements, ephemeralFailedLifecycleV58(object.sql))
		}
	}
	return strings.Join(statements, ";")
}
