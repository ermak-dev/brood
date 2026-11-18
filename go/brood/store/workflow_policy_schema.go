package store

import (
	"context"
	"database/sql"
	"strings"
)

// Версия не ссылается на изменяемые identities: удаление проекта/автора не стирает историю.
const workflowPolicySchema = `CREATE TABLE project_workflow_policy_versions (
 project_id TEXT NOT NULL CHECK (length(project_id) > 0),
 revision INTEGER NOT NULL CHECK (revision > 0),
 rules TEXT NOT NULL CHECK (length(rules) BETWEEN 2 AND 131072 AND json_valid(rules) AND json_type(rules) = 'array'),
 created_by TEXT NOT NULL CHECK (length(created_by) > 0),
 created_at TEXT NOT NULL CHECK (length(created_at) > 0),
 PRIMARY KEY (project_id, revision)
);
CREATE TABLE project_workflow_policy_heads (
 project_id TEXT PRIMARY KEY NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL CHECK (revision > 0),
 FOREIGN KEY (project_id, revision) REFERENCES project_workflow_policy_versions(project_id, revision)
);`

var workflowPolicyTriggers = []struct{ name, sql string }{
	{"project_workflow_policy_heads_advance", `CREATE TRIGGER project_workflow_policy_heads_advance BEFORE UPDATE ON project_workflow_policy_heads WHEN NEW.project_id IS NOT OLD.project_id OR NEW.revision != OLD.revision + 1 BEGIN SELECT RAISE(ABORT, 'workflow policy head requires a new revision'); END`},
	{"project_workflow_policy_heads_no_delete", `CREATE TRIGGER project_workflow_policy_heads_no_delete BEFORE DELETE ON project_workflow_policy_heads WHEN EXISTS (SELECT 1 FROM projects WHERE id=OLD.project_id) BEGIN SELECT RAISE(ABORT, 'workflow policy head requires a new revision'); END`},
	{"project_workflow_policy_heads_no_replace", `CREATE TRIGGER project_workflow_policy_heads_no_replace BEFORE INSERT ON project_workflow_policy_heads WHEN EXISTS (SELECT 1 FROM project_workflow_policy_heads WHERE project_id=NEW.project_id) BEGIN SELECT RAISE(ABORT, 'workflow policy head requires a new revision'); END`},
	{"project_workflow_policy_versions_no_update", `CREATE TRIGGER project_workflow_policy_versions_no_update BEFORE UPDATE ON project_workflow_policy_versions BEGIN SELECT RAISE(ABORT, 'workflow policy version is immutable'); END`},
	{"project_workflow_policy_versions_no_delete", `CREATE TRIGGER project_workflow_policy_versions_no_delete BEFORE DELETE ON project_workflow_policy_versions BEGIN SELECT RAISE(ABORT, 'workflow policy version is immutable'); END`},
	{"project_workflow_policy_versions_no_replace", `CREATE TRIGGER project_workflow_policy_versions_no_replace BEFORE INSERT ON project_workflow_policy_versions WHEN EXISTS (SELECT 1 FROM project_workflow_policy_versions WHERE project_id=NEW.project_id AND revision=NEW.revision) BEGIN SELECT RAISE(ABORT, 'workflow policy version is immutable'); END`},
}

func workflowPolicyFullSchema() string {
	statements := []string{workflowPolicySchema}
	for _, trigger := range workflowPolicyTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}
func verifyWorkflowPolicySchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, workflowPolicySchema); err != nil {
		return err
	}
	for _, trigger := range workflowPolicyTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}
