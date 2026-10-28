// FEATURE: Atomic editable user properties and deletion without orphaned swarms.
// SPEC: Keep one active admin, revoke changed privileges, and retain live resources.
package store

import (
	"errors"
	"testing"
)

func TestUserPropertiesAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	if err := s.CreateUser(ctx, User{ID: "root", Username: "root", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserProperties(ctx, "root", "Changed", false, true); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("last admin: %v", err)
	}
	u, _ := s.GetUser(ctx, "root")
	if u.DisplayName != nil || !u.IsAdmin || u.Disabled {
		t.Fatalf("partial write after refusal: %+v", u)
	}
	seedUser(t, s, "member", "member")
	if err := s.CreateSession(ctx, Session{TokenHash: "session", UserID: "member"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserProperties(ctx, "member", "Member", true, false); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUser(ctx, "member")
	if u.DisplayName == nil || *u.DisplayName != "Member" || !u.IsAdmin || u.Disabled {
		t.Fatalf("update: %+v", u)
	}
	if _, err := s.GetSession(ctx, "session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("privilege change retained session: %v", err)
	}
	if err := s.UpdateUserProperties(ctx, "missing", "", false, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestProfileOnlyUpdatePreservesSession(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedUser(t, s, "u", "u")
	if err := s.CreateSession(ctx, Session{TokenHash: "s", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserProperties(ctx, "u", "New name", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "s"); err != nil {
		t.Fatalf("profile update revoked session: %v", err)
	}
	if err := s.UpdateUserProperties(ctx, "u", "New name", false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocked session: %v", err)
	}
}

func TestSafeUserDeletion(t *testing.T) {
	for _, state := range []string{"provisioning", "active", "error", "deleting", "deleted"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			seedProjectWithMember(t, s, "p", "u")
			if err := s.CreateSwarm(ctx, Swarm{ID: "sw", ProjectID: "p", OwnerID: "u", Name: "sw", AtespaceName: "sw", State: state}); err != nil {
				t.Fatal(err)
			}
			err := s.DeleteUserWithoutSwarms(ctx, "u")
			if state != "deleted" {
				if !errors.Is(err, ErrUserOwnsSwarms) {
					t.Fatalf("delete live owner: %v", err)
				}
				if _, err := s.GetUser(ctx, "u"); err != nil {
					t.Fatalf("owner removed: %v", err)
				}
				if _, err := s.GetSwarm(ctx, "sw"); err != nil {
					t.Fatalf("swarm removed: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	s := newTestStore(t)
	ctx := t.Context()
	if err := s.CreateUser(ctx, User{ID: "a", Username: "a", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUserWithoutSwarms(ctx, "a"); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("last admin delete: %v", err)
	}
	if err := s.DeleteUserWithoutSwarms(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete: %v", err)
	}
	seedUser(t, s, "empty", "empty")
	if err := s.DeleteUserWithoutSwarms(ctx, "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUser(ctx, "empty"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty user remains: %v", err)
	}
}
