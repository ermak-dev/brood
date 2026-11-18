package store

import (
	"context"
	"database/sql"
	"strings"
)

// Схема 35 заморожена: дальнейшие изменения требуют новой версии и миграции.
// История вмешательств отделена от автоматических workspace slots: owner
// не теряет управление, неизвестные эффекты не забываются.
const actorInterventionTables = `CREATE TABLE actor_interventions (
 id TEXT PRIMARY KEY NOT NULL,
 project_id TEXT NOT NULL,
 swarm_id TEXT NOT NULL,
 actor_uid TEXT NOT NULL,
 incarnation INTEGER NOT NULL CHECK (incarnation>0),
 human_id TEXT NOT NULL,
 caller TEXT NOT NULL,
 operation TEXT NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('inflight','unconfirmed','completed')),
 created_at TEXT NOT NULL
);
CREATE TABLE actor_step_interruptions (
 intervention_id TEXT NOT NULL REFERENCES actor_interventions(id) ON DELETE RESTRICT,
 swarm_id TEXT NOT NULL,
 sequence INTEGER NOT NULL,
 PRIMARY KEY (intervention_id,swarm_id,sequence),
 FOREIGN KEY (swarm_id,sequence) REFERENCES swarm_steps(swarm_id,sequence) ON DELETE RESTRICT
)`

var actorInterventionObjects = []struct{ kind, name, sql string }{
	{"index", "actor_interventions_actor", `CREATE INDEX actor_interventions_actor ON actor_interventions(actor_uid,state)`},
	{"trigger", "actor_interventions_fence", `CREATE TRIGGER actor_interventions_fence BEFORE UPDATE ON actor_interventions WHEN
 NEW.id IS NOT OLD.id OR NEW.project_id IS NOT OLD.project_id OR NEW.swarm_id IS NOT OLD.swarm_id OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.incarnation IS NOT OLD.incarnation OR NEW.human_id IS NOT OLD.human_id OR NEW.caller IS NOT OLD.caller OR NEW.operation IS NOT OLD.operation OR NEW.created_at IS NOT OLD.created_at
 OR OLD.state!='inflight' OR NEW.state NOT IN ('completed','unconfirmed')
 BEGIN SELECT RAISE(ABORT,'owner intervention identity and settled history are immutable'); END`},
	{"trigger", "actor_interventions_no_delete", `CREATE TRIGGER actor_interventions_no_delete BEFORE DELETE ON actor_interventions BEGIN SELECT RAISE(ABORT,'owner intervention history is retained'); END`},
	{"trigger", "actor_step_interruptions_fence", `CREATE TRIGGER actor_step_interruptions_fence BEFORE UPDATE ON actor_step_interruptions BEGIN SELECT RAISE(ABORT,'step interruption history is immutable'); END`},
	{"trigger", "actor_step_interruptions_no_delete", `CREATE TRIGGER actor_step_interruptions_no_delete BEFORE DELETE ON actor_step_interruptions BEGIN SELECT RAISE(ABORT,'step interruption history is retained'); END`},
	{"trigger", "swarm_steps_owner_interruption", `CREATE TRIGGER swarm_steps_owner_interruption BEFORE UPDATE ON swarm_steps WHEN
 (NEW.state IN ('completed','answered','stopped','refused') OR NEW.finished_at IS NOT NULL OR NEW.outcome IS NOT OLD.outcome)
 AND EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.swarm_id=OLD.swarm_id AND i.sequence=OLD.sequence)
 BEGIN SELECT RAISE(ABORT,'owner-interrupted execution requires explicit reconciliation'); END`},
}

func actorInterventionSchema() string {
	statements := []string{actorInterventionTables}
	for _, o := range actorInterventionObjects {
		statements = append(statements, o.sql)
	}
	return strings.Join(statements, ";")
}

func verifyActorInterventionSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, actorInterventionTables); err != nil {
		return err
	}
	for _, o := range actorInterventionObjects {
		if err := checkObject(ctx, tx, o.kind, o.name, o.sql); err != nil {
			return err
		}
	}
	return nil
}
