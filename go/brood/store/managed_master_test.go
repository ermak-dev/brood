package store

// FEATURE: Durable managed role and human delegation.
// SPEC: Legacy actors become workers, delegation is immutable, and reopening keeps it.
// IMPLEMENTATION: Additive schema 7 role, delegator and parent UID columns.
// RESULT: IMPLEMENTED — legacy task evidence and durable delegation survive reopen.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestManagedMasterV6Migration(t *testing.T) {
	ctx := context.Background()
	path := lifecycleSchema3(t)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE managed_actors ADD COLUMN ` + managedStageColumn + `; ` + managedConnectionsRuntimeSchema() + `; PRAGMA user_version=6`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.GetManagedActor(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if a.Role != "worker" || a.DelegatorID != "" || a.ParentUID != "" || a.UPID != "UPID:unknown" || a.OperationID != "old-create" {
			t.Fatalf("migration lost identity: %+v", a)
		}
		s.Close()
	}
}

func TestManagedMasterPersistenceAndImmutability(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "roles.sqlite3")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	managedFixture(t, s)
	if err := s.SetUserAdmin(ctx, "u", true); err != nil {
		t.Fatal(err)
	}
	request := managedRequest("master", "one", 101)
	request.Role = "master"
	request.DelegatorID = "u"
	master, err := s.ReserveManagedActor(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request = managedRequest("child", "one", 102)
	request.ParentUID = master.UID
	child, err := s.ReserveManagedActor(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, want := range []ManagedActor{master, child} {
		got, err := s.GetManagedActor(ctx, want.UID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Role != want.Role || got.DelegatorID != want.DelegatorID || got.ParentUID != want.ParentUID {
			t.Fatalf("delegation lost: %+v", got)
		}
		for _, mutate := range []func(*ManagedActor){func(a *ManagedActor) {
			a.Role = "master"
			if want.Role == "master" {
				a.Role = "worker"
			}
		}, func(a *ManagedActor) { a.DelegatorID = "other" }, func(a *ManagedActor) { a.ParentUID = "other" }} {
			changed := got
			mutate(&changed)
			if _, err := s.UpdateManagedActor(ctx, changed, got.Revision); !errors.Is(err, ErrManagedBinding) {
				t.Fatalf("delegation mutable: %v", err)
			}
		}
	}
}

func TestManagedMasterLegacyDefaults(t *testing.T) {
	ctx := context.Background()
	path := lifecycleSchema3(t)
	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.GetManagedActor(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if a.Role != "worker" || a.DelegatorID != "" || a.ParentUID != "" {
			t.Fatalf("unsafe legacy delegation: %+v", a)
		}
		s.Close()
	}
}
