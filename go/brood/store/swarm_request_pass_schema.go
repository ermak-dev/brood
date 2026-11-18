package store

import (
	"context"
	"database/sql"
	"strings"
)

const swarmRequestPassSchema = `CREATE TABLE swarm_request_pass_contexts (
 request_id TEXT PRIMARY KEY NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
 swarm_id TEXT NOT NULL CHECK (length(swarm_id)>0),
 pass_sequence INTEGER NOT NULL CHECK (pass_sequence>0),
 observed_sequence INTEGER NOT NULL CHECK (observed_sequence>=pass_sequence)
);`

const swarmRequestPassIndex = `CREATE INDEX swarm_request_pass_contexts_pass ON swarm_request_pass_contexts(swarm_id,pass_sequence,observed_sequence,request_id)`

var swarmRequestPassTriggers = []struct{ name, sql string }{
	{"swarm_request_pass_contexts_no_update", `CREATE TRIGGER swarm_request_pass_contexts_no_update BEFORE UPDATE ON swarm_request_pass_contexts BEGIN SELECT RAISE(ABORT, 'request pass context is immutable'); END`},
	{"swarm_request_pass_contexts_no_delete", `CREATE TRIGGER swarm_request_pass_contexts_no_delete BEFORE DELETE ON swarm_request_pass_contexts BEGIN SELECT RAISE(ABORT, 'request pass context history is immutable'); END`},
	{"swarm_request_pass_contexts_no_replace", `CREATE TRIGGER swarm_request_pass_contexts_no_replace BEFORE INSERT ON swarm_request_pass_contexts WHEN EXISTS(SELECT 1 FROM swarm_request_pass_contexts WHERE request_id=NEW.request_id) BEGIN SELECT RAISE(ABORT, 'request pass context is immutable'); END`},
	{"swarm_request_pass_contexts_insert", `CREATE TRIGGER swarm_request_pass_contexts_insert BEFORE INSERT ON swarm_request_pass_contexts WHEN NOT EXISTS(
 SELECT 1 FROM swarm_requests r JOIN swarms sw ON sw.id=r.swarm_id
 JOIN swarm_steps st ON st.swarm_id=sw.id AND st.sequence=NEW.observed_sequence
 JOIN swarm_steps p ON p.swarm_id=sw.id AND p.sequence=NEW.pass_sequence AND p.goal IS NOT NULL
 WHERE r.id=NEW.request_id AND r.swarm_id=NEW.swarm_id AND r.origin='actor'
 AND sw.state='active' AND sw.execution IN ('running','waiting')
 AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=sw.id)
 AND p.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=sw.id AND goal IS NOT NULL AND sequence<=st.sequence))
 BEGIN SELECT RAISE(ABORT, 'request pass context is not current'); END`},
}

func swarmRequestPassFullSchema() string {
	statements := []string{swarmRequestPassSchema, swarmRequestPassIndex}
	for _, trigger := range swarmRequestPassTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}

func verifySwarmRequestPassSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, swarmRequestPassSchema); err != nil {
		return err
	}
	if err := checkObject(ctx, tx, "index", "swarm_request_pass_contexts_pass", swarmRequestPassIndex); err != nil {
		return err
	}
	for _, trigger := range swarmRequestPassTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}
