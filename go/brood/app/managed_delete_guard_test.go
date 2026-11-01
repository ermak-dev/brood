package app

// FEATURE: Managed inventory guards swarm deletion under the project lock.
// SPEC: Retained managed allocations or uncertain cleanup refuse before local
// deletion intent and before native atespace mutation.

import (
	"errors"
	"testing"
)

type managedDeleteStore struct {
	Store
	blocked bool
	calls   int
}

func (s *managedDeleteStore) CheckSwarmDeletion(id string) error {
	s.calls++
	if s.blocked {
		return ErrSwarmNotEmpty
	}
	return nil
}

func TestManagedInventoryGuardsSwarmDeletion(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	p := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, p, "one")
	guard := &managedDeleteStore{Store: e.store, blocked: true}
	e.svc.store = guard
	if err := e.svc.DeleteSwarm(idAlice, sw.ID); !errors.Is(err, ErrSwarmNotEmpty) {
		t.Fatalf("managed allocation did not refuse deletion: %v", err)
	}
	current, _, _ := e.store.SwarmByID(sw.ID)
	if current.State != StateActive || current.Retry != "" {
		t.Fatalf("guard changed swarm intent: %+v", current)
	}
	if uid, err := e.prov.GetAtespace(sw.AtespaceName); err != nil || uid != sw.AtespaceUID {
		t.Fatal("guard allowed native deletion")
	}
	guard.blocked = false
	if err := e.svc.DeleteSwarm(idAlice, sw.ID); err != nil {
		t.Fatal(err)
	}
	if guard.calls != 2 {
		t.Fatalf("guard calls %d", guard.calls)
	}
}
