package store

import "strings"

// Зарегистрированная очередь занимает один слот: история intent остаётся навсегда.
const ephemeralChildLogicalSlots = `(SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=NEW.swarm_id AND c.parent_sequence=NEW.parent_sequence)+(SELECT count(*) FROM ephemeral_child_intents e WHERE e.swarm_id=NEW.swarm_id AND e.parent_sequence=NEW.parent_sequence AND NOT EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=e.swarm_id AND c.parent_sequence=e.parent_sequence AND c.request_id=e.request_id))`

// Прямой SQL может конвертировать только точную тройку intent/allocation/proof.
// Локальная authority остаётся обязательной отдельной проверкой Store API.
const ephemeralChildConversion = `EXISTS(SELECT 1 FROM ephemeral_child_intents e
 JOIN ephemeral_child_allocations a ON a.intent_id=e.id
 JOIN ephemeral_child_attestations proof ON proof.intent_id=a.intent_id
 JOIN swarm_child_joins j ON j.swarm_id=e.swarm_id AND j.parent_sequence=e.parent_sequence
 JOIN swarms sw ON sw.id=e.swarm_id
 JOIN swarm_steps p ON p.swarm_id=e.swarm_id AND p.sequence=e.parent_sequence
 JOIN swarm_steps first ON first.swarm_id=e.swarm_id AND first.sequence=e.pass
 JOIN pipeline_sources src ON src.swarm_id=e.swarm_id
 JOIN managed_actors actor ON actor.uid=a.actor_uid
 JOIN managed_actors parent ON parent.uid=e.parent_uid
 JOIN managed_pools pool ON pool.uid=e.pool_uid
 WHERE e.swarm_id=NEW.swarm_id AND e.parent_sequence=NEW.parent_sequence AND e.request_id=NEW.request_id AND e.payload=NEW.payload
 AND a.actor_uid=NEW.actor_uid AND a.actor_incarnation=NEW.actor_incarnation
 AND NEW.role='' AND NEW.delivery='' AND NEW.delivery_detail=''
 AND proof.actor_uid=a.actor_uid AND proof.actor_incarnation=a.actor_incarnation AND proof.state='attested'
 AND proof.origin=e.origin AND proof.source_commit=e.source_commit AND proof.attested_origin=e.origin AND proof.attested_commit=e.source_commit
 AND j.pass=e.pass AND j.project_id=e.project_id AND j.atespace_uid=e.atespace_uid AND j.parent_uid=e.parent_uid AND j.parent_incarnation=e.parent_incarnation AND j.parent_request_id=e.parent_request_id AND j.state='open'
 AND sw.project_id=e.project_id AND sw.atespace_uid=e.atespace_uid AND sw.state='active' AND sw.execution IN ('running','waiting')
 AND p.actor_uid=e.parent_uid AND p.actor_incarnation=e.parent_incarnation AND p.request_id=e.parent_request_id AND p.attested_commit=e.source_commit AND p.state IN ('delivering','inflight','unconfirmed')
 AND p.sequence=(SELECT max(sequence) FROM swarm_steps WHERE swarm_id=e.swarm_id)
 AND e.pass=(SELECT max(sequence) FROM swarm_steps WHERE swarm_id=e.swarm_id AND sequence<=p.sequence AND goal IS NOT NULL)
 AND first.source_commit=e.source_commit AND src.origin=e.origin
 AND actor.actor_class='ephemeral' AND actor.role='worker' AND actor.parent_uid=e.parent_uid AND actor.swarm_id=e.swarm_id AND actor.atespace_uid=e.atespace_uid AND actor.incarnation=a.actor_incarnation
 AND actor.pool_uid=e.pool_uid AND actor.origin=e.origin AND actor.template=e.template AND actor.source_fingerprint=e.source_fingerprint
 AND actor.phase='running' AND actor.desired='running' AND actor.stage='done' AND actor.operation='start'
 AND parent.swarm_id=e.swarm_id AND parent.atespace_uid=e.atespace_uid AND parent.incarnation=e.parent_incarnation AND parent.phase='running' AND parent.desired='running' AND parent.stage='done' AND parent.operation='start'
 AND pool.state='enabled' AND pool.template=e.template AND pool.source_fingerprint=e.source_fingerprint
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.swarm_id=e.swarm_id AND i.sequence=e.parent_sequence)
 AND NOT EXISTS(SELECT 1 FROM actor_interventions i WHERE i.actor_uid=a.actor_uid AND i.incarnation=a.actor_incarnation)
 AND NOT EXISTS(SELECT 1 FROM coordination_claims c WHERE c.kind='workspace' AND c.slot=a.actor_uid))`

var ephemeralChildIntentObjects = func() []struct{ name, kind, sql string } {
	objects := append([]struct{ name, kind, sql string }(nil), ephemeralChildIntentObjectsV47...)
	objects[3].sql = `CREATE TRIGGER ephemeral_child_intent_insert_guard BEFORE INSERT ON ephemeral_child_intents WHEN NOT EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=NEW.swarm_id AND j.parent_sequence=NEW.parent_sequence AND j.pass=NEW.pass AND j.project_id=NEW.project_id AND j.atespace_uid=NEW.atespace_uid AND j.parent_uid=NEW.parent_uid AND j.parent_incarnation=NEW.parent_incarnation AND j.parent_request_id=NEW.parent_request_id AND j.state='open') OR EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=NEW.swarm_id AND c.parent_sequence=NEW.parent_sequence AND c.request_id=NEW.request_id) OR ` + ephemeralChildLogicalSlots + `>=(SELECT total FROM swarm_child_joins j WHERE j.swarm_id=NEW.swarm_id AND j.parent_sequence=NEW.parent_sequence) BEGIN SELECT RAISE(ABORT,'ephemeral child slot conflicts'); END`
	objects[4].sql = `CREATE TRIGGER ephemeral_child_task_insert_guard BEFORE INSERT ON swarm_child_tasks WHEN
 (EXISTS(SELECT 1 FROM ephemeral_child_intents e WHERE e.swarm_id=NEW.swarm_id AND e.parent_sequence=NEW.parent_sequence AND e.request_id=NEW.request_id)
 OR EXISTS(SELECT 1 FROM managed_actors a WHERE a.uid=NEW.actor_uid AND a.actor_class='ephemeral')
 OR EXISTS(SELECT 1 FROM ephemeral_child_allocations a WHERE a.actor_uid=NEW.actor_uid)
 OR ` + ephemeralChildLogicalSlots + `>= (SELECT total FROM swarm_child_joins j WHERE j.swarm_id=NEW.swarm_id AND j.parent_sequence=NEW.parent_sequence)) AND NOT ` + ephemeralChildConversion + `
 BEGIN SELECT RAISE(ABORT,'ephemeral child slot conflicts'); END`
	return objects
}()

func ephemeralChildRegistrationMigration() string {
	var statements []string
	for _, object := range ephemeralChildIntentObjects[3:] {
		statements = append(statements, "DROP TRIGGER "+object.name, object.sql)
	}
	return strings.Join(statements, ";")
}
