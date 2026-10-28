package app

import (
	"errors"
	"testing"
)

// FEATURE: Reserve shard swarm slots before native provisioning (#48).
// SPEC: Every non-deleted state consumes a slot; an authorized same-name retry
// is idempotent even at the limit; administrators do not bypass capacity.
type capacityTestStore struct {
	Store
	capacity      ShardCapacity
	capacityError error
	reads         int
}

func (s *capacityTestStore) ProjectCapacity(projectID string) (ShardCapacity, error) {
	s.reads++
	return s.capacity, s.capacityError
}

func TestCreateSwarmChecksShardCapacity(t *testing.T) {
	for _, actor := range []string{idAlice, idAdmin} {
		t.Run(actor, func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			st := &capacityTestStore{Store: e.store, capacity: ShardCapacity{MaxSwarms: capacityLimit(0)}}
			e.svc.store = st
			_, err := e.svc.CreateSwarm(actor, project, "denied")
			var exceeded *CapacityExceededError
			if !errors.As(err, &exceeded) || exceeded.Resource != "swarms" || exceeded.Limit != 0 || exceeded.Used != 0 || exceeded.Requested != 1 {
				t.Fatalf("wrong capacity denial: %v", err)
			}
			rows, _ := e.store.SwarmsByProject(project)
			if len(rows) != 0 {
				t.Fatal("denied create reserved a row")
			}
			if len(e.prov.byName) != 0 {
				t.Fatal("denied create reached native provisioning")
			}
		})
	}
}

func TestCreateSwarmAllLiveStatesConsumeCapacity(t *testing.T) {
	for _, state := range []SwarmState{StateActive, StateProvisioning, StateError, StateDeleting, SwarmState("future")} {
		t.Run(string(state), func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			if err := e.store.SaveSwarm(Swarm{ID: "occupied", ProjectID: project, OwnerID: idAlice, Name: "existing", AtespaceName: "swarm-occupied", State: state}); err != nil {
				t.Fatal(err)
			}
			e.svc.store = &capacityTestStore{Store: e.store, capacity: ShardCapacity{MaxSwarms: capacityLimit(1)}}
			if _, err := e.svc.CreateSwarm(idAlice, project, "new"); err == nil {
				t.Fatal("non-deleted swarm did not consume a slot")
			}
		})
	}
}

func TestCreateSwarmRetryAtCapacityAndDeletedSlotReuse(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice)
	st := &capacityTestStore{Store: e.store, capacity: ShardCapacity{MaxSwarms: capacityLimit(1)}}
	e.svc.store = st
	first := mustCreateSwarm(t, e, idAlice, project, "first")
	st.capacity.MaxSwarms = capacityLimit(0)
	same, err := e.svc.CreateSwarm(idAlice, project, "first")
	if err != nil || same.ID != first.ID {
		t.Fatalf("same-name retry consumed a slot: %+v, %v", same, err)
	}
	if err := e.svc.DeleteSwarm(idAlice, first.ID); err != nil {
		t.Fatal(err)
	}
	st.capacity.MaxSwarms = capacityLimit(1)
	if _, err := e.svc.CreateSwarm(idAlice, project, "second"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateSwarmCapacityReadFailsClosedAfterAuthorization(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice)
	sentinel := errors.New("capacity unavailable")
	st := &capacityTestStore{Store: e.store, capacityError: sentinel}
	e.svc.store = st
	if _, err := e.svc.CreateSwarm(idBob, project, "foreign"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("unauthorized result: %v", err)
	}
	if st.reads != 0 {
		t.Fatal("unauthorized create read quota data")
	}
	if _, err := e.svc.CreateSwarm(idAlice, project, "denied"); !errors.Is(err, sentinel) {
		t.Fatalf("capacity read failed open: %v", err)
	}
	if len(e.prov.byName) != 0 {
		t.Fatal("capacity read failure reached native provisioning")
	}
}
