package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// BUG: Cascading project deletion can lose bindings to live native resources (#66).
// HYPOTHESIS: The safe deletion guard must share the write transaction with DELETE.
// SOLUTION: A BEGIN IMMEDIATE guard refuses every non-deleted swarm before cascading.
// RESULT: FIXED — every live state is preserved; 100 two-connection race repetitions pass.
func TestSafeProjectDeletionPreservesLiveSwarmBindings(t *testing.T) {
	for _, state := range []string{"provisioning", "active", "error", "deleting", "future-state", "deleted"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			seedSwarm(t, s, "sw", "p", "u", "alpha")
			if err := s.SetSwarmState(ctx, "sw", state); err != nil {
				t.Fatal(err)
			}
			err := s.DeleteProjectWithoutSwarms(ctx, "p")
			if state == "deleted" {
				if err != nil {
					t.Fatalf("delete tombstone-only project: %v", err)
				}
				if _, err := s.GetProject(ctx, "p"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("deleted project = %v", err)
				}
				if _, err := s.GetSwarm(ctx, "sw"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("tombstone remains: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrProjectHasSwarms) {
				t.Fatalf("project with %s swarm = %v, want refusal", state, err)
			}
			if _, err := s.GetProject(ctx, "p"); err != nil {
				t.Fatalf("refusal lost project: %v", err)
			}
			sw, err := s.GetSwarm(ctx, "sw")
			if err != nil || sw.State != state {
				t.Fatalf("refusal changed swarm: %+v, %v", sw, err)
			}
			if role, err := s.GetSwarmRole(ctx, "sw", "u"); err != nil || role != "owner" {
				t.Fatalf("refusal lost owner role: %q, %v", role, err)
			}
			if member, err := s.IsMember(ctx, "p", "u"); err != nil || !member {
				t.Fatalf("refusal lost project membership: %v, %v", member, err)
			}
		})
	}
}

func TestSafeProjectDeletionEmptyAndMissing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProjectWithMember(t, s, "p", "u")
	if err := s.DeleteProjectWithoutSwarms(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUser(ctx, "u"); err != nil {
		t.Fatalf("project deletion removed user: %v", err)
	}
	if err := s.DeleteProjectWithoutSwarms(ctx, "p"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project = %v", err)
	}
}

func TestProjectDeletionRacesWithSwarmCreation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	seedProjectWithMember(t, first, "p", "u")
	start := make(chan struct{})
	created, deleted := make(chan error, 1), make(chan error, 1)
	go func() {
		<-start
		created <- first.CreateSwarm(ctx, Swarm{ID: "sw", ProjectID: "p", OwnerID: "u", Name: "alpha", AtespaceName: "swarm-sw"})
	}()
	go func() {
		<-start
		deleted <- second.DeleteProjectWithoutSwarms(ctx, "p")
	}()
	close(start)
	createErr, deleteErr := <-created, <-deleted
	if createErr == nil {
		if !errors.Is(deleteErr, ErrProjectHasSwarms) {
			t.Fatalf("successful create raced with delete: %v", deleteErr)
		}
		if _, err := first.GetSwarm(ctx, "sw"); err != nil {
			t.Fatalf("successful create lost binding: %v", err)
		}
	} else {
		if !IsForeignKeyViolation(createErr) || deleteErr != nil {
			t.Fatalf("race outcomes: create=%v delete=%v", createErr, deleteErr)
		}
		if _, err := first.GetProject(ctx, "p"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("successful delete retained project: %v", err)
		}
	}
}
