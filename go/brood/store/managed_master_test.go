package store

// FEATURE: Durable managed role and human delegation.
// SPEC: Delegation is immutable, survives reopen, and a foreign schema
// version is refused without mutation.
// IMPLEMENTATION: Additive schema 7 role, delegator and parent UID columns.
// RESULT: IMPLEMENTED — legacy task evidence and durable delegation survive reopen.
// STATUS: DONE

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestManagedMasterForeignVersionRefused: миграций нет (#205); база
// версии 6 не обновляется, а отвергается без изменения схемы и версии.
func TestManagedMasterForeignVersionRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "roles.sqlite3")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version=6`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, s.db)
	s.Close()
	if st, err := Open(ctx, path); err == nil {
		st.Close()
		t.Fatal("previous managed master version accepted")
	} else if !strings.Contains(err.Error(), "incompatible database schema") {
		t.Fatal(err)
	}
	if err := assertDatabaseUnchanged(t, path, 6, before); err != nil {
		t.Fatal(err)
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

// TestManagedMasterReserveDefaults: новый актор без явной делегации —
// worker без delegator и parent; значение переживает переоткрытие.
func TestManagedMasterReserveDefaults(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "roles.sqlite3")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	managedFixture(t, s)
	if _, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.GetManagedActor(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Role != "worker" || a.DelegatorID != "" || a.ParentUID != "" {
		t.Fatalf("unsafe delegation default: %+v", a)
	}
}
