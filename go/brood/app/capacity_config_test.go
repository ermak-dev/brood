package app

import (
	"errors"
	"testing"
)

// FEATURE: Only active administrators configure shard capacity (#48).
// SPEC: Project members may read settings, but not change them; a lowered limit
// changes future admission without deleting or suspending existing workloads.
func (s *capacityTestStore) SetProjectCapacity(actorID, projectID string, capacity ShardCapacity) error {
	s.capacity = capacity
	return nil
}

func TestShardCapacityConfigurationAuthorization(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice)
	st := &capacityTestStore{Store: e.store}
	e.svc.store = st
	cfg := ShardCapacity{MaxActors: capacityLimit(0), WorkerSelector: map[string]string{"shard": "one"}}
	if err := e.svc.SetShardCapacity(idAlice, project, cfg); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("member configured capacity: %v", err)
	}
	if err := e.svc.SetShardCapacity(idAdmin, project, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.ShardCapacity(idAlice, project)
	if err != nil || got.MaxActors == nil || *got.MaxActors != 0 || got.WorkerSelector["shard"] != "one" {
		t.Fatalf("settings not readable by member: %+v, %v", got, err)
	}
	if _, err := e.svc.ShardCapacity(idBob, project); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("outsider read capacity: %v", err)
	}
	user, _, _ := e.store.UserByID(idAdmin)
	user.Disabled = true
	if err := e.store.UpdateUser(user); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetShardCapacity(idAdmin, project, ShardCapacity{}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("disabled admin configured capacity: %v", err)
	}
	if _, err := e.svc.ShardCapacity(idAdmin, project); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("disabled admin read capacity: %v", err)
	}
}

func TestShardCapacityConfigurationMissingProject(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	if err := e.svc.SetShardCapacity(idAdmin, "missing", ShardCapacity{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project write: %v", err)
	}
	if _, err := e.svc.ShardCapacity(idAdmin, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project read: %v", err)
	}
}
