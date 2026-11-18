package store

import (
	"context"
	"database/sql"
	"strings"
)

// Identity и physical fingerprint остаются в immutable allocation; ledger их не дублирует.
const ephemeralChildCreateGrantTable = `CREATE TABLE ephemeral_child_create_grants (
 intent_id TEXT PRIMARY KEY NOT NULL REFERENCES ephemeral_child_allocations(intent_id) ON DELETE RESTRICT,
 state TEXT NOT NULL CHECK(state IN ('queued','reserved','legacy')),
 created_at TEXT NOT NULL CHECK(created_at<>''),
 reserved_at TEXT NOT NULL DEFAULT '' CHECK((state='reserved' AND reserved_at<>'') OR (state IN ('queued','legacy') AND reserved_at=''))
)`

var ephemeralChildCreateGrantObjects = []struct{ name, sql string }{
	{"ephemeral_child_create_grant_insert_guard", `CREATE TRIGGER ephemeral_child_create_grant_insert_guard BEFORE INSERT ON ephemeral_child_create_grants WHEN NEW.state!='queued' OR NEW.reserved_at!='' OR NOT EXISTS(SELECT 1 FROM ephemeral_child_allocations a WHERE a.intent_id=NEW.intent_id AND a.created_at=NEW.created_at) OR EXISTS(SELECT 1 FROM ephemeral_child_create_grants WHERE intent_id=NEW.intent_id) BEGIN SELECT RAISE(ABORT,'ephemeral child create grant is immutable'); END`},
	{"ephemeral_child_create_grant_update_guard", `CREATE TRIGGER ephemeral_child_create_grant_update_guard BEFORE UPDATE ON ephemeral_child_create_grants WHEN OLD.state!='queued' OR NEW.state!='reserved' OR NEW.intent_id IS NOT OLD.intent_id OR NEW.created_at IS NOT OLD.created_at OR OLD.reserved_at!='' OR NEW.reserved_at='' BEGIN SELECT RAISE(ABORT,'ephemeral child create grant is immutable'); END`},
	{"ephemeral_child_create_grant_delete_guard", `CREATE TRIGGER ephemeral_child_create_grant_delete_guard BEFORE DELETE ON ephemeral_child_create_grants BEGIN SELECT RAISE(ABORT,'ephemeral child create grant is immutable'); END`},
	// INSERT allocation доступен только после reserveManagedActor; ошибка журнала откатывает обе записи.
	{"ephemeral_child_create_grant_initialize", `CREATE TRIGGER ephemeral_child_create_grant_initialize AFTER INSERT ON ephemeral_child_allocations BEGIN INSERT INTO ephemeral_child_create_grants(intent_id,state,created_at,reserved_at) VALUES(NEW.intent_id,'queued',NEW.created_at,''); END`},
}

func ephemeralChildCreateGrantGuards() string {
	var statements []string
	for _, object := range ephemeralChildCreateGrantObjects {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func ephemeralChildCreateGrantFullSchema() string {
	return ephemeralChildCreateGrantTable + ";" + ephemeralChildCreateGrantGuards()
}

func ephemeralChildCreateGrantMigration() string {
	// Отсутствие истории в 51 не доказывает, что Create не был отправлен.
	// Seed до оград: новые INSERT уже не могут создавать legacy или reserved.
	return ephemeralChildCreateGrantTable + `;INSERT INTO ephemeral_child_create_grants(intent_id,state,created_at,reserved_at) SELECT intent_id,'legacy',created_at,'' FROM ephemeral_child_allocations ORDER BY rowid;` + ephemeralChildCreateGrantGuards()
}

func verifyEphemeralChildCreateGrantSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ephemeralChildCreateGrantTable); err != nil {
		return err
	}
	for _, object := range ephemeralChildCreateGrantObjects {
		if err := checkObject(ctx, tx, "trigger", object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}
