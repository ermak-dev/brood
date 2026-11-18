package store

import (
	"context"
	"database/sql"
	"strings"
)

// Отдельная retained identity не зависит от удаления project/swarm inventory.
const swarmEvalBindingSchema = `CREATE TABLE swarm_eval_bindings (
 candidate_swarm_id TEXT PRIMARY KEY CHECK(length(candidate_swarm_id)>0),
 binding TEXT NOT NULL CHECK(json_valid(binding)),
 nonproduction INTEGER NOT NULL CHECK(nonproduction=1)
);`

var swarmEvalBindingTriggers = []struct{ name, sql string }{
	{"swarm_eval_bindings_no_update", `CREATE TRIGGER swarm_eval_bindings_no_update BEFORE UPDATE ON swarm_eval_bindings BEGIN SELECT RAISE(ABORT,'eval binding is immutable'); END`},
	{"swarm_eval_bindings_no_delete", `CREATE TRIGGER swarm_eval_bindings_no_delete BEFORE DELETE ON swarm_eval_bindings BEGIN SELECT RAISE(ABORT,'eval binding is immutable'); END`},
	{"swarm_eval_bindings_no_replace", `CREATE TRIGGER swarm_eval_bindings_no_replace BEFORE INSERT ON swarm_eval_bindings WHEN EXISTS(SELECT 1 FROM swarm_eval_bindings WHERE candidate_swarm_id=NEW.candidate_swarm_id) BEGIN SELECT RAISE(ABORT,'eval binding is immutable'); END`},
}

const swarmEvalReplaySchema = `CREATE TABLE swarm_eval_replays (
 candidate_swarm_id TEXT NOT NULL REFERENCES swarm_eval_bindings(candidate_swarm_id),
 request_id TEXT NOT NULL CHECK(length(request_id)>0),
 decision TEXT NOT NULL CHECK(json_valid(decision)),
 PRIMARY KEY(candidate_swarm_id,request_id)
);`

var swarmEvalReplayTriggers = []struct{ name, sql string }{
	{"swarm_eval_replays_no_update", `CREATE TRIGGER swarm_eval_replays_no_update BEFORE UPDATE ON swarm_eval_replays BEGIN SELECT RAISE(ABORT,'eval replay is immutable'); END`},
	{"swarm_eval_replays_no_delete", `CREATE TRIGGER swarm_eval_replays_no_delete BEFORE DELETE ON swarm_eval_replays BEGIN SELECT RAISE(ABORT,'eval replay is immutable'); END`},
	{"swarm_eval_replays_no_replace", `CREATE TRIGGER swarm_eval_replays_no_replace BEFORE INSERT ON swarm_eval_replays WHEN EXISTS(SELECT 1 FROM swarm_eval_replays WHERE candidate_swarm_id=NEW.candidate_swarm_id AND request_id=NEW.request_id) BEGIN SELECT RAISE(ABORT,'eval replay is immutable'); END`},
}

func swarmEvalFullSchema() string {
	statements := []string{swarmEvalBindingSchema, swarmEvalReplaySchema}
	for _, t := range append(swarmEvalBindingTriggers, swarmEvalReplayTriggers...) {
		statements = append(statements, t.sql)
	}
	return strings.Join(statements, ";")
}

func verifySwarmEvalSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, swarmEvalBindingSchema); err != nil {
		return err
	}
	if err := checkSchema(ctx, tx, swarmEvalReplaySchema); err != nil {
		return err
	}
	for _, t := range append(swarmEvalBindingTriggers, swarmEvalReplayTriggers...) {
		if err := checkObject(ctx, tx, "trigger", t.name, t.sql); err != nil {
			return err
		}
	}
	return nil
}
