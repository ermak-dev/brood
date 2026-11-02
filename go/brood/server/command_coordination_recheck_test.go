package server

// FEATURE: Fence coordination delivery after relay connectivity checks.
// SPEC: Revocation or target-generation replacement during connectivity probing
// prevents delivery; the durable claim is retained, never automatically released.
// IMPLEMENTATION: Recheck delegation after namespace RPCs, and recheck source,
// target and exact active lease after connectivity probing before delivery.
// RESULT: IMPLEMENTED — late revocation and released-claim regressions pass.
// STATUS: DONE

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type coordinationCheckingRelay struct {
	guestRelay
	check func()
}

func (h coordinationCheckingRelay) Connected(string, int64) bool { h.check(); return true }

func TestCoordinationDoesNotDeliverAfterClaimRelease(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	ts.srv.managed.hub = coordinationCheckingRelay{guestRelay: ts.srv.managed.hub, check: func() {
		claim, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, worker.UID)
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.srv.store.ReleaseDispatchLease(ctx, claim); err != nil {
			t.Fatal(err)
		}
	}}
	calls := 0
	ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		calls++
		return relay.MessageResponse{OK: true}, nil
	}
	inv := coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "released-before-delivery"})
	if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, inv); err == nil || calls != 0 {
		t.Fatalf("released claim delivered: %v calls=%d", err, calls)
	}
	if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, inv); err != nil || calls != 0 {
		t.Fatalf("released retry delivered: %v calls=%d", err, calls)
	}
}

func TestCoordinationRechecksAfterNamespaceRead(t *testing.T) {
	for _, action := range []string{"take", "show"} {
		t.Run(action, func(t *testing.T) {
			ts, master, _ := coordinationServerSetup(t)
			ctx := context.Background()
			sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
			if err != nil {
				t.Fatal(err)
			}
			if action == "show" {
				if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation("landing", map[string]any{"action": "take", "request_id": "private-holder"})); err != nil {
					t.Fatal(err)
				}
			}
			revoke := func() {
				if err := ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, false); err != nil {
					t.Error(err)
				}
			}
			ts.fake.mu.Lock()
			if action == "show" {
				// Revoke during the final namespace read, after loading private state.
				ts.fake.beforeNamespaceGet = func() { ts.fake.beforeNamespaceGet = revoke }
			} else {
				ts.fake.beforeNamespaceGet = revoke
			}
			ts.fake.mu.Unlock()
			got, err := ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation("landing", map[string]any{"action": action, "request_id": "late-revocation"}))
			if err == nil || len(got.Data) != 0 {
				t.Fatalf("revoked result: %+v %v", got, err)
			}
			if action == "take" {
				if _, err := ts.srv.store.ShowLandingToken(ctx, sw.ProjectID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("revoked grant reserved token: %v", err)
				}
			}
		})
	}
}

func TestCoordinationReauthorizesImmediatelyBeforeDelivery(t *testing.T) {
	for _, scenario := range []string{"revoked", "target_generation", "namespace"} {
		t.Run(scenario, func(t *testing.T) {
			ts, master, worker := coordinationServerSetup(t)
			ctx := context.Background()
			sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
			if err != nil {
				t.Fatal(err)
			}
			ts.srv.managed.hub = coordinationCheckingRelay{guestRelay: ts.srv.managed.hub, check: func() {
				switch scenario {
				case "revoked":
					if err := ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, false); err != nil {
						t.Fatal(err)
					}
				case "target_generation":
					if _, err := ts.srv.store.BeginManagedActorOperation(ctx, worker.UID, worker.Revision, "reset-worker", "reset"); !errors.Is(err, store.ErrCoordinationConflict) {
						t.Fatalf("active lease did not guard reset: %v", err)
					}
					// Обычный lifecycle закрыт lease; имитируем внешнюю замену поколения.
					if _, err := ts.srv.catalog.db.ExecContext(ctx, `UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, worker.UID); err != nil {
						t.Fatal(err)
					}
				case "namespace":
					ts.fake.mu.Lock()
					ts.fake.atespaces[sw.AtespaceName].Metadata.Uid = "changed"
					ts.fake.mu.Unlock()
				}
			}}
			calls := 0
			ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
				calls++
				return relay.MessageResponse{OK: true}, nil
			}
			_, err = ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "probe-revocation"}))
			if err == nil || calls != 0 {
				t.Fatalf("delivery after %s: err=%v calls=%d", scenario, err, calls)
			}
			claim, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, master.SwarmID, worker.UID)
			if err != nil || claim.Target != (store.CoordinationActor{UID: worker.UID, Generation: worker.Incarnation}) {
				t.Fatalf("lost original claim: %+v %v", claim, err)
			}
		})
	}
}
