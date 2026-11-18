package store

import (
	"context"
	"database/sql"
	"strings"
)

// Связь с шагом RESTRICT сохраняет точную историю даже при удалении роя.
// Инкарнации акторов — снимки без FK: удаление актора их не переписывает.
const swarmChildTables = `CREATE TABLE swarm_child_joins (
 swarm_id TEXT NOT NULL,
 parent_sequence INTEGER NOT NULL CHECK (parent_sequence > 0),
 pass INTEGER NOT NULL CHECK (pass > 0 AND pass <= parent_sequence),
 project_id TEXT NOT NULL,
 atespace_uid TEXT NOT NULL,
 parent_uid TEXT NOT NULL,
 parent_incarnation INTEGER NOT NULL CHECK (parent_incarnation > 0),
 parent_request_id TEXT NOT NULL,
 mode TEXT NOT NULL CHECK (mode IN ('all','any','quorum')),
 total INTEGER NOT NULL CHECK (total BETWEEN 1 AND 64),
 quorum INTEGER NOT NULL CHECK ((mode='quorum' AND quorum BETWEEN 1 AND total) OR (mode IN ('all','any') AND quorum=0)),
 remaining TEXT NOT NULL CHECK (remaining='retain'),
 created_at TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','aborted')),
 revision INTEGER NOT NULL DEFAULT 1,
 aborted_by TEXT,
 aborted_at TEXT,
 CHECK ((state='open' AND revision=1 AND aborted_by IS NULL AND aborted_at IS NULL) OR (state='aborted' AND revision=2 AND aborted_by IS NOT NULL AND length(aborted_by)>0 AND aborted_at IS NOT NULL)),
 PRIMARY KEY (swarm_id,parent_sequence),
 FOREIGN KEY (swarm_id,parent_sequence) REFERENCES swarm_steps(swarm_id,sequence) ON DELETE RESTRICT,
 FOREIGN KEY (swarm_id,pass) REFERENCES swarm_steps(swarm_id,sequence) ON DELETE RESTRICT
);
CREATE TABLE swarm_child_tasks (
 id TEXT PRIMARY KEY NOT NULL,
 swarm_id TEXT NOT NULL,
 parent_sequence INTEGER NOT NULL,
 request_id TEXT NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128 AND request_id NOT GLOB '*[^a-zA-Z0-9_-]*'),
 payload TEXT NOT NULL CHECK (length(CAST(payload AS BLOB)) BETWEEN 1 AND 4096),
 actor_uid TEXT NOT NULL,
 actor_incarnation INTEGER NOT NULL CHECK (actor_incarnation > 0),
 state TEXT NOT NULL CHECK (state IN ('pending','running','completed','cancelled')),
 outcome TEXT CHECK (outcome IN ('success','fail')),
 report TEXT CHECK (report IS NULL OR length(CAST(report AS BLOB)) BETWEEN 1 AND 16384),
 revision INTEGER NOT NULL CHECK (revision > 0),
 created_at TEXT NOT NULL,
 finished_at TEXT,
 role TEXT NOT NULL DEFAULT '',
 delivery TEXT NOT NULL DEFAULT '' CHECK (delivery IN ('','queued','sending','delivered','not_sent','unconfirmed')),
 delivery_detail TEXT NOT NULL DEFAULT '' CHECK (length(CAST(delivery_detail AS BLOB))<=1024),
 CHECK ((role='' AND delivery='') OR (length(role) BETWEEN 1 AND 63 AND delivery!='')),
 CHECK (state!='pending' OR delivery IN ('','queued')),
 CHECK (state!='running' OR delivery IN ('','sending','delivered','not_sent','unconfirmed')),
 UNIQUE (swarm_id,parent_sequence,request_id),
 FOREIGN KEY (swarm_id,parent_sequence) REFERENCES swarm_child_joins(swarm_id,parent_sequence) ON DELETE RESTRICT,
 CHECK ((state='completed' AND outcome IS NOT NULL) OR (state!='completed' AND outcome IS NULL AND report IS NULL)),
 CHECK ((state IN ('completed','cancelled')) = (finished_at IS NOT NULL))
)`

// Отсутствующие слоты держат родителя до явного abort неисполненной группы.
// Даже aborted не скрывает реальных pending/running обязательств.
const swarmChildUnfinished = `((j.state!='aborted' AND EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.swarm_id=j.swarm_id AND i.sequence=j.parent_sequence)) OR (j.state!='aborted' AND ((SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence)+(SELECT count(*) FROM ephemeral_child_failures f JOIN ephemeral_child_intents e ON e.id=f.intent_id WHERE e.swarm_id=j.swarm_id AND e.parent_sequence=j.parent_sequence))<j.total) OR EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence AND c.state IN ('pending','running') AND NOT EXISTS(SELECT 1 FROM ephemeral_child_executor_failures f WHERE f.child_id=c.id)))`

const swarmChildUnfinishedV55 = `((j.state!='aborted' AND EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.swarm_id=j.swarm_id AND i.sequence=j.parent_sequence)) OR (j.state!='aborted' AND ((SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence)+(SELECT count(*) FROM ephemeral_child_failures f JOIN ephemeral_child_intents e ON e.id=f.intent_id WHERE e.swarm_id=j.swarm_id AND e.parent_sequence=j.parent_sequence))<j.total) OR EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence AND c.state IN ('pending','running')))`

var swarmChildObjects = []struct{ name, sql string }{
	{"swarm_child_joins_insert", `CREATE TRIGGER swarm_child_joins_insert BEFORE INSERT ON swarm_child_joins BEGIN
 SELECT CASE WHEN NEW.state!='open' OR NEW.revision!=1 OR NEW.aborted_by IS NOT NULL OR NEW.aborted_at IS NOT NULL THEN RAISE(ABORT,'child join starts open') END;
 SELECT CASE WHEN EXISTS(SELECT 1 FROM swarm_child_joins WHERE swarm_id=NEW.swarm_id AND parent_sequence=NEW.parent_sequence) THEN RAISE(ABORT,'child join already exists') END;
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM swarm_steps p JOIN swarms sw ON sw.id=p.swarm_id
 WHERE p.swarm_id=NEW.swarm_id AND p.sequence=NEW.parent_sequence AND p.actor_uid=NEW.parent_uid AND p.actor_incarnation=NEW.parent_incarnation AND p.request_id=NEW.parent_request_id
 AND p.state IN ('delivering','inflight','unconfirmed') AND p.attested_commit IS NOT NULL
 AND p.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=p.swarm_id)
 AND NEW.pass=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=p.swarm_id AND sequence<=p.sequence AND goal IS NOT NULL)
 AND sw.project_id=NEW.project_id AND sw.atespace_uid=NEW.atespace_uid AND sw.state='active' AND sw.execution IN ('running','waiting')) THEN RAISE(ABORT,'child join requires the current parent attempt') END;
 END`},
	{"swarm_child_joins_immutable", `CREATE TRIGGER swarm_child_joins_immutable BEFORE UPDATE ON swarm_child_joins WHEN
 NEW.swarm_id IS NOT OLD.swarm_id OR NEW.parent_sequence IS NOT OLD.parent_sequence OR NEW.pass IS NOT OLD.pass OR NEW.project_id IS NOT OLD.project_id OR NEW.atespace_uid IS NOT OLD.atespace_uid OR NEW.parent_uid IS NOT OLD.parent_uid OR NEW.parent_incarnation IS NOT OLD.parent_incarnation OR NEW.parent_request_id IS NOT OLD.parent_request_id OR NEW.mode IS NOT OLD.mode OR NEW.total IS NOT OLD.total OR NEW.quorum IS NOT OLD.quorum OR NEW.remaining IS NOT OLD.remaining OR NEW.created_at IS NOT OLD.created_at
 OR OLD.state!='open' OR NEW.state!='aborted' OR NEW.revision!=OLD.revision+1
 OR (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=OLD.swarm_id AND c.parent_sequence=OLD.parent_sequence)>=OLD.total
 OR EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=OLD.swarm_id AND c.parent_sequence=OLD.parent_sequence AND c.state!='cancelled')
 BEGIN SELECT RAISE(ABORT,'child join identity and policy are immutable; only incomplete unstarted abort is allowed'); END`},
	{"swarm_child_joins_no_delete", `CREATE TRIGGER swarm_child_joins_no_delete BEFORE DELETE ON swarm_child_joins BEGIN SELECT RAISE(ABORT,'child join history is retained'); END`},
	{"swarm_child_tasks_insert", `CREATE TRIGGER swarm_child_tasks_insert BEFORE INSERT ON swarm_child_tasks BEGIN
 SELECT CASE WHEN EXISTS(SELECT 1 FROM swarm_child_tasks WHERE id=NEW.id OR (swarm_id=NEW.swarm_id AND parent_sequence=NEW.parent_sequence AND request_id=NEW.request_id)) THEN RAISE(ABORT,'child request already exists') END;
 SELECT CASE WHEN NEW.state!='pending' OR NEW.revision!=1 OR NEW.outcome IS NOT NULL OR NEW.report IS NOT NULL OR NEW.finished_at IS NOT NULL THEN RAISE(ABORT,'child starts as pending intent') END;
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=NEW.swarm_id AND j.parent_sequence=NEW.parent_sequence AND j.state='open' AND (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence)<j.total) THEN RAISE(ABORT,'child join membership is full or missing') END;
 END`},
	{"swarm_child_tasks_fence", `CREATE TRIGGER swarm_child_tasks_fence BEFORE UPDATE ON swarm_child_tasks WHEN
 NEW.id IS NOT OLD.id OR NEW.swarm_id IS NOT OLD.swarm_id OR NEW.parent_sequence IS NOT OLD.parent_sequence OR NEW.request_id IS NOT OLD.request_id OR NEW.payload IS NOT OLD.payload OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.actor_incarnation IS NOT OLD.actor_incarnation OR NEW.created_at IS NOT OLD.created_at
 OR NEW.role IS NOT OLD.role OR NEW.revision!=OLD.revision+1 OR NOT (
 ((OLD.state='pending' AND NEW.state='running' AND ((OLD.delivery='' AND NEW.delivery='') OR (OLD.delivery='queued' AND NEW.delivery='sending')))
 OR (OLD.state='pending' AND NEW.state='cancelled' AND NEW.delivery=OLD.delivery)
 OR (OLD.state='running' AND NEW.state='completed' AND OLD.delivery!='not_sent' AND (NEW.delivery=OLD.delivery OR (OLD.delivery IN ('sending','unconfirmed') AND NEW.delivery='delivered')))
 OR (OLD.state='running' AND NEW.state='cancelled' AND OLD.delivery='not_sent' AND NEW.delivery=OLD.delivery)) AND (NEW.delivery_detail=OLD.delivery_detail OR (OLD.state='running' AND NEW.state='completed' AND NEW.delivery='delivered'))
 OR (OLD.state='running' AND NEW.state=OLD.state AND OLD.delivery='sending' AND NEW.delivery IN ('delivered','not_sent','unconfirmed') AND NEW.outcome IS OLD.outcome AND NEW.report IS OLD.report AND NEW.finished_at IS OLD.finished_at))
 OR EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=OLD.swarm_id AND j.parent_sequence=OLD.parent_sequence AND j.state!='open')
 BEGIN SELECT RAISE(ABORT,'child identity, payload and terminal result are immutable'); END`},
	{"swarm_child_tasks_no_delete", `CREATE TRIGGER swarm_child_tasks_no_delete BEFORE DELETE ON swarm_child_tasks BEGIN SELECT RAISE(ABORT,'child task history is retained'); END`},
	{"swarm_steps_child_identity", `CREATE TRIGGER swarm_steps_child_identity BEFORE UPDATE ON swarm_steps WHEN EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=OLD.swarm_id AND j.parent_sequence=OLD.sequence) AND (NEW.swarm_id IS NOT OLD.swarm_id OR NEW.sequence IS NOT OLD.sequence OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.actor_incarnation IS NOT OLD.actor_incarnation OR NEW.request_id IS NOT OLD.request_id OR NEW.node_id IS NOT OLD.node_id OR NEW.attested_commit IS NOT OLD.attested_commit) BEGIN SELECT RAISE(ABORT,'child parent identity is immutable'); END`},
	{"swarm_steps_child_close", `CREATE TRIGGER swarm_steps_child_close BEFORE UPDATE ON swarm_steps WHEN (NEW.state IN ('completed','answered','stopped','refused') OR NEW.finished_at IS NOT NULL OR NEW.outcome IS NOT OLD.outcome) AND EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=OLD.swarm_id AND j.parent_sequence=OLD.sequence AND ` + swarmChildUnfinished + `) BEGIN SELECT RAISE(ABORT,'unfinished children protect the parent step'); END`},
	{"swarm_steps_child_delete", `CREATE TRIGGER swarm_steps_child_delete BEFORE DELETE ON swarm_steps WHEN EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=OLD.swarm_id AND (j.parent_sequence=OLD.sequence OR j.pass=OLD.sequence)) BEGIN SELECT RAISE(ABORT,'child parent history is retained'); END`},
	{"swarm_steps_child_insert", `CREATE TRIGGER swarm_steps_child_insert BEFORE INSERT ON swarm_steps WHEN EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=NEW.swarm_id AND (j.parent_sequence=NEW.sequence OR ` + swarmChildUnfinished + `)) BEGIN SELECT RAISE(ABORT,'unfinished children protect parent progression'); END`},
	{"swarms_child_close", `CREATE TRIGGER swarms_child_close BEFORE UPDATE ON swarms WHEN (NEW.execution NOT IN ('running','waiting') OR NEW.state IN ('deleting','deleted')) AND EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=OLD.id AND ` + swarmChildUnfinished + ` AND NOT (NEW.execution='stopped' AND NEW.state='active' AND EXISTS(SELECT 1 FROM actor_step_interruptions i JOIN actor_interventions a ON a.id=i.intervention_id WHERE i.swarm_id=j.swarm_id AND i.sequence=j.parent_sequence AND a.operation='pass_stop' AND a.state='completed'))) BEGIN SELECT RAISE(ABORT,'unfinished children protect the parent pass'); END`},
}

const swarmChildRunningIndex = `CREATE UNIQUE INDEX swarm_child_running_actor ON swarm_child_tasks(actor_uid) WHERE state='running'`
const swarmChildQueueIndex = `CREATE INDEX swarm_child_queue_actor ON swarm_child_tasks(actor_uid,state,delivery)`

// Замороженный DDL шага 34→35; failure facts появляются только в шаге 54→55.
func swarmChildrenSchemaV35() string {
	return strings.ReplaceAll(swarmChildrenFullSchema(), swarmChildUnfinished, swarmChildUnfinishedV54)
}

const swarmChildUnfinishedV54 = `((j.state!='aborted' AND EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.swarm_id=j.swarm_id AND i.sequence=j.parent_sequence)) OR (j.state!='aborted' AND (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence)<j.total) OR EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence AND c.state IN ('pending','running')))`

func swarmChildrenFullSchema() string {
	statements := []string{swarmChildTables, swarmChildRunningIndex, swarmChildQueueIndex, actorInterventionSchema()}
	for _, object := range swarmChildObjects {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func verifySwarmChildrenSchema(ctx context.Context, tx *sql.Tx) error {
	if err := verifyActorInterventionSchema(ctx, tx); err != nil {
		return err
	}
	if err := checkSchema(ctx, tx, swarmChildTables); err != nil {
		return err
	}
	for name, ddl := range map[string]string{"swarm_child_running_actor": swarmChildRunningIndex, "swarm_child_queue_actor": swarmChildQueueIndex} {
		if err := checkObject(ctx, tx, "index", name, ddl); err != nil {
			return err
		}
	}
	for _, object := range swarmChildObjects {
		if err := checkObject(ctx, tx, "trigger", object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}
