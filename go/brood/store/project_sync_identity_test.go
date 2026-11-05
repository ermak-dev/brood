package store

import (
	"context"
	"errors"
	"testing"
)

func TestProjectSyncRequiresLockedIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "fixture", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	seedProject(t, s, "current", "project")
	declaration := ProjectDeclaration{Name: "project", Repository: "https://git.example.com/team/repo.git"}
	for _, expected := range []string{"", "stale"} {
		_, err := s.SyncProject(ctx, "admin", expected, declaration, func(context.Context) bool { return true })
		if !errors.Is(err, ErrProjectSyncIdentityChanged) {
			t.Fatalf("unlocked identity %q accepted: %v", expected, err)
		}
		origin, err := s.ProjectRepository(ctx, "current")
		if err != nil || origin != "" {
			t.Fatalf("identity mismatch changed repository: %q %v", origin, err)
		}
	}
	result, err := s.SyncProject(ctx, "admin", "current", declaration, func(context.Context) bool { return true })
	if err != nil || result.ID != "current" || !result.Changed {
		t.Fatalf("locked identity refused: %+v %v", result, err)
	}
	if err := s.DeleteProject(ctx, "current"); err != nil {
		t.Fatal(err)
	}
	_, err = s.SyncProject(ctx, "admin", "current", declaration, func(context.Context) bool { return true })
	if !errors.Is(err, ErrProjectSyncIdentityChanged) {
		t.Fatalf("deleted identity recreated without a fresh lookup: %v", err)
	}
	if _, err := s.ProjectByName(ctx, "project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale identity recreated project: %v", err)
	}
}
