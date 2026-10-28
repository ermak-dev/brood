package app

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// BUG: Atomic nonempty refusal leaves a swarm in error and blocks child cleanup (#71).
// SPEC: Only a typed refusal with the same surviving native UID restores active;
// transport uncertainty and replacement UIDs retain the existing error/retry fence.
func TestDeleteNonemptyRestoresCleanupWithoutDeletingWorkloads(t *testing.T) {
	for _, state := range []SwarmState{StateActive, StateError, StateDeleting} {
		t.Run(string(state), func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			sw := mustCreateSwarm(t, e, idAlice, project, "one")
			if state != StateActive {
				sw.State = state
				sw.Retry = RetryDelete
				if err := e.store.UpdateSwarm(sw); err != nil {
					t.Fatal(err)
				}
			}
			e.prov.failDelete = true
			e.prov.deleteErr = fmt.Errorf("native refusal: %w", ErrSwarmNotEmpty)
			err := e.svc.DeleteSwarm(idAlice, sw.ID)
			if !errors.Is(err, ErrSwarmNotEmpty) {
				t.Fatalf("refusal lost: %v", err)
			}
			got, _, _ := e.store.SwarmByID(sw.ID)
			if got.State != StateActive || got.Retry != "" || got.AtespaceUID != sw.AtespaceUID {
				t.Fatalf("nonempty refusal poisoned cleanup: %+v", got)
			}
			if allowed, err := e.svc.Can(idAlice, ActionManageSwarm, SwarmRes(sw.ID)); err != nil || !allowed {
				t.Fatalf("owner cannot clean up children: %v, %v", allowed, err)
			}
			if uid, err := e.prov.GetAtespace(sw.AtespaceName); err != nil || uid != sw.AtespaceUID {
				t.Fatal("nonempty namespace was changed")
			}
			e.prov.failDelete = false
			if err := e.svc.DeleteSwarm(idAlice, sw.ID); err != nil {
				t.Fatal(err)
			}
			got, _, _ = e.store.SwarmByID(sw.ID)
			if got.State != StateDeleted {
				t.Fatalf("retry did not finish: %+v", got)
			}
		})
	}
}

type blockedNonemptyProvisioner struct {
	*fakeProvisioner
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (p *blockedNonemptyProvisioner) DeleteAtespace(name, uid string) error {
	if p.calls.Add(1) == 1 {
		close(p.entered)
	}
	<-p.release
	return ErrSwarmNotEmpty
}

func TestDeleteWaiterRechecksMembershipBeforeNativeMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.seedUsers(t)
		project := e.seedProject(t, idAlice)
		sw := mustCreateSwarm(t, e, idAlice, project, "one")
		blocked := &blockedNonemptyProvisioner{fakeProvisioner: e.prov, entered: make(chan struct{}), release: make(chan struct{})}
		e.svc.provisioner = blocked
		first, second := make(chan error, 1), make(chan error, 1)
		go func() { first <- e.svc.DeleteSwarm(idAdmin, sw.ID) }()
		<-blocked.entered
		go func() { second <- e.svc.DeleteSwarm(idAlice, sw.ID) }()
		synctest.Wait()
		if blocked.calls.Load() != 1 {
			t.Fatal("concurrent delete bypassed project serialization")
		}
		if err := e.store.RemoveMembership(project, idAlice); err != nil {
			t.Fatal(err)
		}
		close(blocked.release)
		synctest.Wait()
		if err := <-first; !errors.Is(err, ErrSwarmNotEmpty) {
			t.Fatalf("first refusal=%v", err)
		}
		if err := <-second; !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("queued revoked owner=%v", err)
		}
		if blocked.calls.Load() != 1 {
			t.Fatal("revoked queued request reached native delete")
		}
	})
}

func TestDeleteRefusalDoesNotRestoreForeignOrUnknownNamespace(t *testing.T) {
	for _, scenario := range []string{"foreign UID", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			sw := mustCreateSwarm(t, e, idAlice, project, "one")
			e.prov.failDelete = true
			if scenario == "foreign UID" {
				e.prov.deleteErr = ErrSwarmNotEmpty
				e.prov.byName[sw.AtespaceName] = "foreign-uid"
			} else {
				e.prov.deleteErr = errors.New("transport unavailable")
			}
			if err := e.svc.DeleteSwarm(idAlice, sw.ID); err == nil {
				t.Fatal("failed delete reported success")
			}
			got, _, _ := e.store.SwarmByID(sw.ID)
			if got.State != StateError || got.Retry != RetryDelete {
				t.Fatalf("uncertain or foreign outcome restored active: %+v", got)
			}
		})
	}
}
