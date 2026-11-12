package server

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: A human administrator can explicitly recover a revoked master's exact lease.
// SPEC: Recovery never impersonates the actor, resends a task, or releases another request.
func TestRecoveryRechecksSessionAfterNamespacePreflight(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	valid := true
	ctx := context.WithValue(context.Background(), commandSessionAuthorityKey{}, func(context.Context) bool { return valid })
	op, err := ts.srv.store.AcquireActorOperation(ctx, worker.UID, worker.Incarnation, "recover-session", "user:operator", "restart")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.FinishActorOperation(ctx, op, false); err != nil {
		t.Fatal(err)
	}
	ts.fake.beforeNamespaceGet = func() { valid = false }
	inv := coordinationInvocation("dispatch", map[string]any{"to": "worker", "action": "release", "request_id": op.OperationID})
	if _, err := ts.srv.executeOperatorCoordinationCommand(ctx, master.DelegatorID, master.SwarmID, inv); err == nil {
		t.Fatal("revoked session released reservation")
	}
	if _, err := ts.srv.store.CurrentActorOperation(ctx, worker.UID); err != nil {
		t.Fatalf("revoked recovery removed reservation: %v", err)
	}
}

func TestOperatorRecoversExactDispatchLease(t *testing.T) {
	ts, master, worker := ownedWorkerSetup(t)
	ctx := context.Background()
	ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		return relay.MessageResponse{OK: true}, nil
	}
	inv := coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "task-1"})
	if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, inv); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.CreateUser(ctx, store.User{ID: "recovery", Username: "recovery", PasswordHash: "test-only", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, true); err != nil {
		t.Fatal(err)
	}
	release := coordinationInvocation("dispatch", map[string]any{"to": "worker", "request_id": "task-1", "action": "release"})
	if _, err := ts.srv.executeOperatorCommand(ctx, "recovery", master.SwarmID, "user:recovery", release); err != nil {
		t.Fatal(err)
	}
	sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if _, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, worker.UID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease retained: %v", err)
	}
}
