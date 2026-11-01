package store

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Additive schema 4 lifecycle migration from deployed schema 3.
// SPEC: Native rows and unresolved task evidence survive; rejected migrations roll back.
// IMPLEMENTATION: Validate schema 3 before additive ALTER in the opening transaction.
// RESULT: IMPLEMENTED — versions 0/1/2/3 upgrade without resetting data.
// STATUS: DONE
func lifecycleSchema3(t *testing.T) string {
	t.Helper()
	path := legacyACLDatabase(t, false)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := swarmMembersSchema + `; INSERT INTO swarm_members SELECT id,owner_id,'owner' FROM swarms; ` + liveSwarmNameIndex + `; ` + projectCapacitySchema + `; ` + managedSchema
	for _, o := range managedConstraints {
		q += "; " + o.sql
	}
	q += `; INSERT INTO managed_pools VALUES ('pool','pool','connection','{}','fingerprint','{}',2,2000,2048,4096,'{}','enabled',1,'then','then');
 INSERT INTO managed_actors VALUES ('a','a','sw','native','pool','cluster','fingerprint','{}',1,'error','running',1000,1024,2048,101,'old-owner','old-create','create','UPID:unknown','uncertain',5,'then','then',NULL);
 PRAGMA user_version=3`
	if _, err = db.Exec(q); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestManagedLifecycleSchema3Migration(t *testing.T) {
	ctx := context.Background()
	path := lifecycleSchema3(t)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
		t.Fatalf("version %d: %v", version, err)
	}
	a, err := s.GetManagedActor(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Stage != "reconcile" || a.UPID != "UPID:unknown" || a.OperationID != "old-create" || a.Revision != 5 || a.OwnerNonce != "old-owner" || a.Incarnation != 1 {
		t.Fatalf("migration lost evidence: %+v", a)
	}
	if _, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "unsafe-delete", "delete"); err == nil {
		t.Fatal("legacy unknown task replaced")
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	a = lifecycleBegin(t, s, a, "reset")
	if a.Stage != "stop" || a.OwnerNonce != "old-owner" {
		t.Fatalf("legacy reconciled reset: %+v", a)
	}
	u, err := s.GetUser(ctx, "u1")
	if err != nil || u.PasswordHash != "private-hash" {
		t.Fatalf("native data: %+v %v", u, err)
	}
	var data string
	if err = s.db.QueryRow(`SELECT data FROM sessions WHERE token_hash='token-hash'`).Scan(&data); err != nil || data != "session-data" {
		t.Fatalf("session: %q %v", data, err)
	}
}
func TestManagedLifecycleLegacyCleanupReconciliation(t *testing.T) {
	for _, operation := range []string{"delete", "reset"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			path := lifecycleSchema3(t)
			db, err := sql.Open("sqlite", dsn(path))
			if err != nil {
				t.Fatal(err)
			}
			desired := "running"
			if operation == "delete" {
				desired = "deleted"
			}
			_, err = db.Exec(`UPDATE managed_actors SET operation=?,desired=?`, operation, desired)
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			a, err := s.GetManagedActor(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID); err == nil {
				t.Fatal("legacy task bypassed reconciliation")
			}
			if operation == "reset" {
				if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "stopped"); err == nil {
					t.Fatal("legacy premature ownership rotation was silently adopted")
				}
				got, err := s.GetManagedActor(ctx, a.UID)
				if err != nil || !reflect.DeepEqual(a, got) {
					t.Fatalf("legacy reset evidence changed: %+v %v", got, err)
				}
				return
			}
			a = lifecycleConfirm(t, s, a, "running")
			if a.Stage != "stop" || a.UPID != "" {
				t.Fatalf("legacy deletion did not schedule owned stop: %+v", a)
			}
			a = lifecycleBind(t, s, a, "UPID:legacy-stop")
			a = lifecycleConfirm(t, s, a, "stopped")
			a = lifecycleBind(t, s, a, "UPID:legacy-delete")
			if _, err = s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedLifecycleSchema3Rollback(t *testing.T) {
	path := lifecycleSchema3(t)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP INDEX managed_actors_live_ctid`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, db)
	db.Close()
	if s, err := Open(context.Background(), path); err == nil {
		s.Close()
		t.Fatal("damaged schema accepted")
	} else if !strings.Contains(err.Error(), "incompatible database schema") {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 3 || !reflect.DeepEqual(before, schemaSnapshot(t, db)) {
		t.Fatalf("failed upgrade mutated schema: %d %v", v, err)
	}
	var upid string
	if err = db.QueryRow(`SELECT upid FROM managed_actors`).Scan(&upid); err != nil || upid != "UPID:unknown" {
		t.Fatalf("lost task: %q %v", upid, err)
	}
}
