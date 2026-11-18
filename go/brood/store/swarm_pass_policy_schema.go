package store

import (
	"context"
	"database/sql"
	"strings"
)

// Snapshot сохраняет identity после удаления роя/шага; версия policy имеет свой FK.
const swarmPassPolicySchema = `CREATE TABLE swarm_pass_policies (
 swarm_id TEXT NOT NULL CHECK (length(swarm_id) > 0),
 pass_sequence INTEGER NOT NULL CHECK (pass_sequence > 0),
 project_id TEXT NOT NULL CHECK (length(project_id) > 0),
 policy_revision INTEGER CHECK (policy_revision IS NULL OR policy_revision > 0),
 legacy_empty INTEGER NOT NULL CHECK (legacy_empty IN (0,1) AND (legacy_empty=0 OR policy_revision IS NULL)),
 PRIMARY KEY (swarm_id, pass_sequence),
 FOREIGN KEY (project_id, policy_revision) REFERENCES project_workflow_policy_versions(project_id, revision)
);`

const legacyPassPolicies = `INSERT INTO swarm_pass_policies(swarm_id,pass_sequence,project_id,policy_revision,legacy_empty) SELECT st.swarm_id,st.sequence,sw.project_id,NULL,1 FROM swarm_steps st JOIN swarms sw ON sw.id=st.swarm_id WHERE st.goal IS NOT NULL ORDER BY st.rowid;`

var swarmPassPolicyTriggers = []struct{ name, sql string }{
	{"swarm_pass_policies_insert", `CREATE TRIGGER swarm_pass_policies_insert BEFORE INSERT ON swarm_pass_policies WHEN NEW.legacy_empty != 0 OR NOT EXISTS (SELECT 1 FROM swarm_steps st JOIN swarms sw ON sw.id=st.swarm_id WHERE st.swarm_id=NEW.swarm_id AND st.sequence=NEW.pass_sequence AND st.goal IS NOT NULL AND sw.project_id=NEW.project_id) OR NEW.policy_revision IS NOT (SELECT revision FROM project_workflow_policy_heads WHERE project_id=NEW.project_id) BEGIN SELECT RAISE(ABORT, 'swarm pass policy must match admitted project policy'); END`},
	{"swarm_pass_policies_no_update", `CREATE TRIGGER swarm_pass_policies_no_update BEFORE UPDATE ON swarm_pass_policies BEGIN SELECT RAISE(ABORT, 'swarm pass policy is immutable'); END`},
	{"swarm_pass_policies_no_delete", `CREATE TRIGGER swarm_pass_policies_no_delete BEFORE DELETE ON swarm_pass_policies BEGIN SELECT RAISE(ABORT, 'swarm pass policy is immutable'); END`},
	{"swarm_pass_policies_no_replace", `CREATE TRIGGER swarm_pass_policies_no_replace BEFORE INSERT ON swarm_pass_policies WHEN EXISTS (SELECT 1 FROM swarm_pass_policies WHERE swarm_id=NEW.swarm_id AND pass_sequence=NEW.pass_sequence) BEGIN SELECT RAISE(ABORT, 'swarm pass policy is immutable'); END`},
}

func swarmPassPolicyFullSchema() string { return swarmPassPolicyDDL(false) }
func swarmPassPolicyMigration() string  { return swarmPassPolicyDDL(true) }
func swarmPassPolicyDDL(backfill bool) string {
	statements := []string{swarmPassPolicySchema}
	if backfill {
		statements = append(statements, legacyPassPolicies)
	}
	for _, trigger := range swarmPassPolicyTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}
func verifySwarmPassPolicySchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, swarmPassPolicySchema); err != nil {
		return err
	}
	for _, trigger := range swarmPassPolicyTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}
