package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed gitlab.sql
var gitlabSchema string

// История не ссылается внешними ключами на удаляемых пользователей, акторов
// и рои: отзыв и удаление не стирают личность и неопределённый эффект.
const gitlabGrantFence = `CREATE TRIGGER gitlab_grants_fence BEFORE UPDATE ON gitlab_grants WHEN
 NEW.id IS NOT OLD.id OR NEW.project_id IS NOT OLD.project_id OR NEW.swarm_id IS NOT OLD.swarm_id OR
 NEW.atespace_uid IS NOT OLD.atespace_uid OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.incarnation IS NOT OLD.incarnation OR
 NEW.human_id IS NOT OLD.human_id OR NEW.binding_revision IS NOT OLD.binding_revision OR
 NEW.can_read IS NOT OLD.can_read OR NEW.can_create IS NOT OLD.can_create OR NEW.can_propose IS NOT OLD.can_propose OR
 NEW.expires_at IS NOT OLD.expires_at OR OLD.revoked != 0 OR NEW.revoked != 1 OR NEW.revision != OLD.revision + 1
 BEGIN SELECT RAISE(ABORT, 'GitLab grant is immutable except revocation'); END`

const gitlabOperationFence = `CREATE TRIGGER gitlab_operations_fence BEFORE UPDATE ON gitlab_operations WHEN
 NEW.id IS NOT OLD.id OR NEW.project_id IS NOT OLD.project_id OR NEW.swarm_id IS NOT OLD.swarm_id OR
 NEW.atespace_uid IS NOT OLD.atespace_uid OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.incarnation IS NOT OLD.incarnation OR
 NEW.kind IS NOT OLD.kind OR NEW.request_id IS NOT OLD.request_id OR NEW.grant_id IS NOT OLD.grant_id OR
 NEW.binding_revision IS NOT OLD.binding_revision OR NEW.binding_snapshot IS NOT OLD.binding_snapshot OR NEW.payload_hash IS NOT OLD.payload_hash OR NEW.snapshot IS NOT OLD.snapshot OR
 NEW.created_at IS NOT OLD.created_at OR NEW.revision != OLD.revision + 1 OR NOT (
 (OLD.state = 'reserved' AND NEW.state IN ('sending', 'refused')) OR
 (OLD.state = 'sending' AND NEW.state IN ('completed', 'refused', 'unknown')))
 BEGIN SELECT RAISE(ABORT, 'GitLab operation identity or transition is invalid'); END`

const gitlabApprovalFence = `CREATE TRIGGER gitlab_approvals_fence BEFORE UPDATE ON gitlab_approvals WHEN
 NEW.operation_id IS NOT OLD.operation_id OR NEW.human_id IS NOT OLD.human_id OR NEW.session_hash IS NOT OLD.session_hash OR
 NEW.expires_at IS NOT OLD.expires_at OR OLD.state != 'approved' OR NEW.state NOT IN ('revoked', 'claimed') OR NEW.revision != OLD.revision + 1
 BEGIN SELECT RAISE(ABORT, 'GitLab approval is immutable except claim or revocation'); END`

func gitlabTriggers() map[string]string {
	triggers := map[string]string{"gitlab_grants_fence": gitlabGrantFence, "gitlab_operations_fence": gitlabOperationFence, "gitlab_approvals_fence": gitlabApprovalFence}
	for _, table := range []string{"gitlab_grants", "gitlab_operations", "gitlab_approvals"} {
		key := "id"
		if table == "gitlab_approvals" {
			key = "operation_id"
		}
		triggers[table+"_no_delete"] = fmt.Sprintf(`CREATE TRIGGER %s_no_delete BEFORE DELETE ON %s BEGIN SELECT RAISE(ABORT, 'GitLab history cannot be deleted'); END`, table, table)
		identity := key + "=NEW." + key
		if table == "gitlab_operations" {
			// REPLACE по второму UNIQUE не вызывает DELETE trigger при
			// выключенном recursive_triggers: защищаем и scoped request key.
			identity += " OR (project_id=NEW.project_id AND actor_uid=NEW.actor_uid AND incarnation=NEW.incarnation AND kind=NEW.kind AND request_id=NEW.request_id)"
		}
		triggers[table+"_no_replace"] = fmt.Sprintf(`CREATE TRIGGER %s_no_replace BEFORE INSERT ON %s WHEN EXISTS (SELECT 1 FROM %s WHERE %s) BEGIN SELECT RAISE(ABORT, 'GitLab history cannot be replaced'); END`, table, table, table, identity)
	}
	return triggers
}

func gitlabFullSchema() string {
	ddl := gitlabSchema
	// Фиксированный порядок объектов нужен стабильным fixtures.
	triggers := gitlabTriggers()
	for _, name := range gitlabTriggerNames() {
		ddl += ";" + triggers[name]
	}
	return ddl
}

func gitlabTriggerNames() []string {
	return []string{"gitlab_grants_fence", "gitlab_operations_fence", "gitlab_approvals_fence", "gitlab_grants_no_delete", "gitlab_grants_no_replace", "gitlab_operations_no_delete", "gitlab_operations_no_replace", "gitlab_approvals_no_delete", "gitlab_approvals_no_replace"}
}

func verifyGitLabSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, gitlabSchema); err != nil {
		return err
	}
	for name, ddl := range gitlabTriggers() {
		if err := checkObject(ctx, tx, "trigger", name, ddl); err != nil {
			return err
		}
	}
	return nil
}
