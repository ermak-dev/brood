package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Fail-closed managed allocation and restart recovery.
// SPEC: Unknown creates cannot start/reset; operation observations survive restart;
// aliases share CTID uniqueness, all resource ceilings apply, tombstones allow reuse.
func TestManagedUnknownOperationCannotRestart(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"provisioning", "error", "deleting"} {
		a.Phase = phase
		a, err = s.UpdateManagedActor(ctx, a, a.Revision)
		if err != nil {
			t.Fatal(err)
		}
		for _, operation := range []string{"start", "reset"} {
			if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "unsafe", operation); err == nil {
				t.Fatalf("%s accepted for unknown %s", operation, phase)
			}
		}
	}
}
func TestManagedDeleteIntentCannotBeCanceledByObservation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleConfirm(t, s, a, "absent")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "delete-a", "delete")
	if err != nil {
		t.Fatal(err)
	}
	a.Phase = "stopped"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"start", "reset", "stop"} {
		if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "cancel-delete", operation); err == nil {
			t.Fatalf("%s canceled deletion intent", operation)
		}
	}
}

func TestManagedPendingIntentBlocksAnotherStart(t *testing.T) {
	for _, operation := range []string{"start", "reset", "stop"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			managedFixture(t, s)
			a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
			if err != nil {
				t.Fatal(err)
			}
			a.Phase = "stopped"
			a, err = s.UpdateManagedActor(ctx, a, a.Revision)
			if err != nil {
				t.Fatal(err)
			}
			a = lifecycleConfirm(t, s, a, "stopped")
			a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "intent", operation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "replacement", "start"); err == nil {
				t.Fatal("unreconciled intent was superseded by start")
			}
		})
	}
}

func TestManagedDurableIntentAndFences(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inventory.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	p := managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	a.UPID = "UPID:create"
	a.Error = "public error"
	a.Phase = "error"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetManagedActor(ctx, a.UID)
	if err != nil || !reflect.DeepEqual(a, got) {
		t.Fatalf("recovery=%+v err=%v", got, err)
	}
	gotPool, err := s.GetManagedPool(ctx, p.UID)
	if err != nil || !reflect.DeepEqual(p, gotPool) {
		t.Fatalf("pool recovery=%+v err=%v", gotPool, err)
	}
	changed := a
	changed.UPID = "UPID:replacement"
	if _, err := s.UpdateManagedActor(ctx, changed, a.Revision); err == nil {
		t.Fatal("UPID replacement accepted")
	}
	if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, a.OperationID, "delete"); err == nil {
		t.Fatal("operation ID rebound")
	}
	a.Phase = "stopped"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	oldRevision := a.Revision
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "reset-1", "reset")
	if err != nil {
		t.Fatal(err)
	}
	if a.Incarnation != 1 || a.Stage != "stop" || a.OwnerNonce != "owner-a" || a.UPID != "" || a.Error != "" || a.Desired != "running" {
		t.Fatalf("reset intent=%+v", a)
	}
	same, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "reset-1", "reset")
	if err != nil || !reflect.DeepEqual(a, same) {
		t.Fatalf("idempotent=%+v err=%v", same, err)
	}
	if _, err := s.ConfirmManagedActorDeleted(ctx, a.UID, oldRevision, "create-a"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale cleanup=%v", err)
	}
	if _, err := s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, "reset-1"); err == nil {
		t.Fatal("non-delete intent released allocation")
	}
}
func TestManagedResourceCeilingsAndValidation(t *testing.T) {
	for _, dimension := range []string{"actors", "cpu", "memory", "disk"} {
		t.Run(dimension, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			p := managedFixture(t, s)
			switch dimension {
			case "actors":
				p.MaxActors = 0
			case "cpu":
				p.Resources.CPUMillicores = 999
			case "memory":
				p.Resources.MemoryBytes = 1023
			case "disk":
				p.Resources.DiskBytes = 2047
			}
			if _, err := s.UpdateManagedPool(ctx, p, p.Revision); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101)); !errors.Is(err, ErrPoolCapacity) {
				t.Fatalf("capacity=%v", err)
			}
		})
	}
	ctx := context.Background()
	s := newTestStore(t)
	p := managedFixture(t, s)
	for _, change := range []func(*ManagedPool){
		func(p *ManagedPool) { p.Source.Endpoint = "https://user:private@pve.invalid" },
		func(p *ManagedPool) { p.Source.Endpoint = "https://pve.invalid/?secret=private" },
		func(p *ManagedPool) { p.Source.CAFingerprint = "-----BEGIN PRIVATE KEY-----" },
		func(p *ManagedPool) { p.Template.URI = "https://user:private@pve.invalid/template" },
		func(p *ManagedPool) { p.Resources.DiskBytes = -1 },
		func(p *ManagedPool) { p.Template.Digest = "sha256:bad" },
	} {
		bad := p
		change(&bad)
		if _, err := s.UpdateManagedPool(ctx, bad, p.Revision); err == nil {
			t.Fatalf("invalid public snapshot accepted: %+v", bad)
		}
	}
	if _, err := s.CreateManagedPool(ctx, ManagedPool{}); err == nil {
		t.Fatal("empty pool accepted")
	}
	if _, err := s.ReserveManagedActor(ctx, ManagedActor{}); err == nil {
		t.Fatal("empty actor accepted")
	}
	bad := managedRequest("bad", "one", 101)
	bad.AtespaceUID = "stale"
	if _, err := s.ReserveManagedActor(ctx, bad); err == nil {
		t.Fatal("stale swarm binding accepted")
	}
	p.Resources = ManagedResources{CPUMillicores: math.MaxInt64, MemoryBytes: math.MaxInt64, DiskBytes: math.MaxInt64}
	p, err := s.UpdateManagedPool(ctx, p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	first := managedRequest("huge", "one", 101)
	first.Resources = p.Resources
	if _, err := s.ReserveManagedActor(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveManagedActor(ctx, managedRequest("overflow", "two", 102)); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("overflow reservation=%v", err)
	}
	small := p
	small.Resources.CPUMillicores--
	if _, err := s.UpdateManagedPool(ctx, small, p.Revision); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("shrink=%v", err)
	}
	if _, err := s.UpdateManagedPool(ctx, p, p.Revision-1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("pool CAS=%v", err)
	}
}
func TestManagedClusterCollisionAndConfirmedReuse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := managedFixture(t, s)
	alias := p
	alias.UID = "alias"
	alias.Name = "alias"
	alias.Connection = "alias"
	alias.Source.Node = "another-node"
	if _, err := s.CreateManagedPool(ctx, alias); err != nil {
		t.Fatal(err)
	}
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	b := managedRequest("b", "two", 101)
	b.PoolUID = "alias"
	if _, err := s.ReserveManagedActor(ctx, b); !IsUniqueViolation(err) {
		t.Fatalf("cluster CTID collision=%v", err)
	}
	if _, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM swarms WHERE id='one'`); err == nil {
		t.Fatal("deleted swarm with live managed allocation")
	}
	if err := s.DeleteProject(ctx, "one"); err == nil {
		t.Fatal("project cascaded managed allocation")
	}
	a = lifecycleConfirm(t, s, a, "absent")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "delete-a", "delete")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, "wrong"); err == nil {
		t.Fatal("unrelated cleanup released allocation")
	}
	a = lifecycleConfirm(t, s, a, "absent")
	a, err = s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	sameName := managedRequest("new", "one", 101)
	sameName.Name = a.Name
	if _, err := s.ReserveManagedActor(ctx, sameName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveManagedActor(ctx, b); !IsUniqueViolation(err) {
		t.Fatalf("reused CTID not reserved=%v", err)
	}
	actors, err := s.ListManagedActors(ctx, "one")
	if err != nil || len(actors) != 2 {
		t.Fatalf("tombstone list=%+v err=%v", actors, err)
	}
}
func TestManagedDamagedSchemaRefusedWithoutMutation(t *testing.T) {
	for _, change := range []string{`DROP INDEX managed_actors_live_ctid`, `DROP TABLE managed_actors`, `DROP TRIGGER managed_swarm_delete_guard`, `ALTER TABLE managed_actors DROP COLUMN stage`, `PRAGMA user_version=13`, `ALTER TABLE managed_actors DROP COLUMN role`, `ALTER TABLE managed_actors DROP COLUMN delegator_id`, `ALTER TABLE managed_actors DROP COLUMN parent_uid`, `ALTER TABLE managed_connections DROP COLUMN runtime_config`} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db.sqlite3")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, s.db)
			s.Close()
			if st, err := Open(ctx, path); err == nil {
				st.Close()
				t.Fatal("damaged lifecycle schema accepted")
			} else if !strings.Contains(err.Error(), "incompatible database schema") {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", dsn(path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if !reflect.DeepEqual(before, schemaSnapshot(t, db)) {
				t.Fatal("rejected schema mutated")
			}
		})
	}
}
