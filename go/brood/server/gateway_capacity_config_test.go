package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// FEATURE: Capacity settings require current administrator rights at persistence (#48).
// SPEC: Demotion between the service check and gateway write cannot change limits.
type interleavedCapacityStore struct {
	app.Store
	beforeSet func()
}

func (s *interleavedCapacityStore) SetProjectCapacity(actorID, projectID string, capacity app.ShardCapacity) error {
	s.beforeSet()
	return s.Store.SetProjectCapacity(actorID, projectID, capacity)
}

func TestGatewayCapacitySettingsRecheckAdministrator(t *testing.T) {
	g, db, prov := swarmRaceFixture(t)
	ctx := context.Background()
	if err := db.CreateUser(ctx, store.User{ID: "other-admin", Username: "other-admin", PasswordHash: "unused", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	st := &interleavedCapacityStore{Store: g, beforeSet: func() {
		if err := db.SetUserAdmin(ctx, "admin", false); err != nil {
			t.Fatal(err)
		}
	}}
	svc := app.NewService(st, prov, time.Now, nil)
	zero := int64(0)
	if err := svc.SetShardCapacity("admin", "p", app.ShardCapacity{MaxActors: &zero}); !errors.Is(err, app.ErrNotAllowed) {
		t.Fatalf("stale administrator changed capacity: %v", err)
	}
	got, err := db.GetProjectCapacity(ctx, "p")
	if err != nil || got.MaxActors != nil {
		t.Fatalf("refused update changed settings: %+v, %v", got, err)
	}
}

func TestGatewayCapacitySettingsValidateAndReplace(t *testing.T) {
	g, db, _ := swarmRaceFixture(t)
	zero := int64(0)
	if err := g.SetProjectCapacity("admin", "p", app.ShardCapacity{MaxActors: &zero}); err != nil {
		t.Fatal(err)
	}
	if err := g.SetProjectCapacity("admin", "p", app.ShardCapacity{WorkerSelector: map[string]string{"bad key": "one"}}); !errors.Is(err, app.ErrInvalidCapacity) {
		t.Fatalf("invalid labels error is not mapped: %v", err)
	}
	got, err := db.GetProjectCapacity(context.Background(), "p")
	if err != nil || got.MaxActors == nil || *got.MaxActors != 0 {
		t.Fatalf("invalid input mutated settings: %+v, %v", got, err)
	}
	if err := g.SetProjectCapacity("alice", "p", app.ShardCapacity{}); !errors.Is(err, app.ErrNotAllowed) {
		t.Fatalf("ordinary user changed capacity: %v", err)
	}
	if err := g.SetProjectCapacity("admin", "missing", app.ShardCapacity{}); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing project error is not mapped: %v", err)
	}
}
