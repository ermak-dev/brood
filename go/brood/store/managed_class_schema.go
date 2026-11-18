package store

import (
	"context"
	"database/sql"
	"strings"
)

// ActorClassPersistent identifies a long-lived actor admitted through the ordinary API.
const ActorClassPersistent = "persistent"

// ActorClassEphemeral identifies server-created inventory for one child task.
const ActorClassEphemeral = "ephemeral"

const managedActorClassColumn = `actor_class TEXT NOT NULL DEFAULT 'persistent' CHECK (actor_class IN ('persistent','ephemeral') AND (actor_class='persistent' OR (role='worker' AND delegator_id='' AND parent_uid<>'')))`

func managedActorClassSchema() string {
	return strings.TrimSuffix(strings.TrimSpace(managedOriginSchema()), ");") + ", " + managedActorClassColumn + ");"
}

var managedActorClassGuards = []struct{ name, sql string }{
	{"managed_actor_class_update_guard", `CREATE TRIGGER managed_actor_class_update_guard BEFORE UPDATE OF actor_class ON managed_actors WHEN OLD.actor_class<>NEW.actor_class BEGIN SELECT RAISE(ABORT,'managed actor class is immutable'); END`},
	{"managed_actor_class_replace_guard", `CREATE TRIGGER managed_actor_class_replace_guard BEFORE INSERT ON managed_actors WHEN EXISTS(SELECT 1 FROM managed_actors WHERE uid=NEW.uid AND actor_class<>NEW.actor_class) BEGIN SELECT RAISE(ABORT,'managed actor class is immutable'); END`},
	{"ephemeral_role_insert_guard", `CREATE TRIGGER ephemeral_role_insert_guard BEFORE INSERT ON swarm_role_bindings WHEN EXISTS(SELECT 1 FROM managed_actors WHERE uid=NEW.actor_uid AND actor_class='ephemeral') BEGIN SELECT RAISE(ABORT,'ephemeral actors cannot bind workflow roles'); END`},
	{"ephemeral_role_update_guard", `CREATE TRIGGER ephemeral_role_update_guard BEFORE UPDATE OF actor_uid ON swarm_role_bindings WHEN EXISTS(SELECT 1 FROM managed_actors WHERE uid=NEW.actor_uid AND actor_class='ephemeral') BEGIN SELECT RAISE(ABORT,'ephemeral actors cannot bind workflow roles'); END`},
}

func managedActorClassGuardSchema() string {
	var statements []string
	for _, guard := range managedActorClassGuards {
		statements = append(statements, guard.sql)
	}
	return strings.Join(statements, ";")
}

func verifyManagedActorClassGuards(ctx context.Context, tx *sql.Tx) error {
	for _, guard := range managedActorClassGuards {
		if err := checkObject(ctx, tx, "trigger", guard.name, guard.sql); err != nil {
			return err
		}
	}
	return nil
}
