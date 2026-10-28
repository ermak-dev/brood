package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// FEATURE: Persist swarm ACL roles while retaining at least one owner (#65).
// SPEC: Creation/transfer are atomic; only owner/editor/viewer are accepted;
// owner_id remains a compatible projection and concurrent changes cannot orphan a swarm.
// IMPLEMENTATION: Role changes and the primary-owner projection share BEGIN IMMEDIATE.
// RESULT: IMPLEMENTED — constraints, extension/reopen and two-connection races are covered.
func TestSwarmMembersAndLastOwner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedSwarm(t, s, "sw", "p", "u1", "alpha")
	seedUser(t, s, "u2", "bob")
	role, err := s.GetSwarmRole(ctx, "sw", "u1")
	if err != nil || role != "owner" {
		t.Fatalf("creator role = %q, %v", role, err)
	}
	if _, err := s.GetSwarmRole(ctx, "sw", "u2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted member = %v, want not found", err)
	}
	for _, role := range []string{"editor", "viewer"} {
		if err := s.SetSwarmMember(ctx, "sw", "u2", role); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSwarmRole(ctx, "sw", "u2")
		if err != nil || got != role {
			t.Fatalf("stored role = %q, %v, want %q", got, err, role)
		}
	}
	if err := s.SetSwarmMember(ctx, "sw", "u2", "ownre"); !errors.Is(err, ErrInvalidSwarmRole) {
		t.Fatalf("invalid role = %v", err)
	}
	if err := s.SetSwarmMember(ctx, "sw", "missing", "viewer"); err == nil || !IsForeignKeyViolation(err) {
		t.Fatalf("missing user = %v, want FK violation", err)
	}
	if err := s.SetSwarmMember(ctx, "missing", "u2", "viewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing swarm = %v", err)
	}
	if err := s.SetSwarmMember(ctx, "sw", "u1", "editor"); !errors.Is(err, ErrLastSwarmOwner) {
		t.Fatalf("last owner demotion = %v", err)
	}
	if err := s.RemoveSwarmMember(ctx, "sw", "u1"); !errors.Is(err, ErrLastSwarmOwner) {
		t.Fatalf("last owner removal = %v", err)
	}
	members, err := s.ListSwarmMembers(ctx, "sw")
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %+v, %v", members, err)
	}
	if err := s.SetSwarmMember(ctx, "sw", "u2", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSwarmMember(ctx, "sw", "u1", "viewer"); err != nil {
		t.Fatal(err)
	}
	sw, err := s.GetSwarm(ctx, "sw")
	if err != nil || sw.OwnerID != "u2" {
		t.Fatalf("compatible owner = %q, %v", sw.OwnerID, err)
	}
	if err := s.RemoveSwarmMember(ctx, "sw", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveSwarmMember(ctx, "sw", "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing member removal = %v", err)
	}
}

func TestSwarmTransferUpdatesRoles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedSwarm(t, s, "sw", "p", "u1", "alpha")
	seedUser(t, s, "u2", "bob")
	if err := s.SetSwarmOwner(ctx, "sw", "u2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSwarmRole(ctx, "sw", "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old owner's grant survives transfer: %v", err)
	}
	role, err := s.GetSwarmRole(ctx, "sw", "u2")
	if err != nil || role != "owner" {
		t.Fatalf("new owner = %q, %v", role, err)
	}
	if err := s.SetSwarmOwner(ctx, "sw", "u2"); err != nil {
		t.Fatalf("same-owner transfer: %v", err)
	}
}

func TestConcurrentSwarmOwnerDemotion(t *testing.T) {
	for _, mutation := range []string{"demote", "remove", "mixed"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "shared.db")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			other, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			seedSwarm(t, s, "sw", "p", "u1", "alpha")
			seedUser(t, s, "u2", "bob")
			if err := s.SetSwarmMember(ctx, "sw", "u2", "owner"); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for index, user := range []string{"u1", "u2"} {
				connection := []*Store{s, other}[index]
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if mutation == "remove" || (mutation == "mixed" && user == "u1") {
						results <- connection.RemoveSwarmMember(ctx, "sw", user)
					} else {
						results <- connection.SetSwarmMember(ctx, "sw", user, "viewer")
					}
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			success, refused := 0, 0
			for err := range results {
				if err == nil {
					success++
				} else if errors.Is(err, ErrLastSwarmOwner) {
					refused++
				} else {
					t.Fatal(err)
				}
			}
			if success != 1 || refused != 1 {
				t.Fatalf("demotions: success=%d refused=%d", success, refused)
			}
			members, err := s.ListSwarmMembers(ctx, "sw")
			if err != nil {
				t.Fatal(err)
			}
			owners := 0
			for _, m := range members {
				if m.Role == "owner" {
					owners++
				}
			}
			if owners != 1 {
				t.Fatalf("remaining owners = %d, want 1", owners)
			}
		})
	}
}

func TestSwarmNameUniqueWithinLiveProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedSwarm(t, s, "sw1", "p", "u1", "alpha")
	second := Swarm{ID: "sw2", ProjectID: "p", OwnerID: "u1", Name: "alpha", AtespaceName: "swarm-sw2"}
	if err := s.CreateSwarm(ctx, second); err == nil || !IsUniqueViolation(err) {
		t.Fatalf("duplicate live name = %v, want unique violation", err)
	}
	if err := s.SetSwarmState(ctx, "sw1", "deleted"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSwarm(ctx, second); err != nil {
		t.Fatalf("reuse deleted name: %v", err)
	}
}

func TestSafeUserDeletionSeesEveryOwnerRole(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedSwarm(t, s, "sw", "p", "u1", "alpha")
	seedUser(t, s, "u2", "bob")
	if err := s.SetSwarmMember(ctx, "sw", "u2", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUserWithoutSwarms(ctx, "u2"); !errors.Is(err, ErrUserOwnsSwarms) {
		t.Fatalf("co-owner deletion = %v, want ownership refusal", err)
	}
}
