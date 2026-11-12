package server

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: Dispatch ownership has an explicit authenticated release utility.
// SPEC: A request ID binds the task content; only its owner can release its allocation.
func TestCommandDispatchReleaseAndPayloadBinding(t *testing.T) {
	ts, master, worker := ownedWorkerSetup(t)
	ctx := context.Background()
	calls := 0
	ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		calls++
		return relay.MessageResponse{OK: true}, nil
	}
	inv := coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "task-1"})
	if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, inv); err != nil {
		t.Fatal(err)
	}
	changed := coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "different task", "request_id": "task-1"})
	if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, changed); err == nil {
		t.Fatal("request ID accepted changed task")
	}
	release := coordinationInvocation("dispatch", map[string]any{"to": "worker", "request_id": "task-1", "action": "release"})
	if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, release); err != nil {
		t.Fatal(err)
	}
	sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if _, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, worker.UID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease retained: %v", err)
	}
	if calls != 1 {
		t.Fatalf("release resent task: %d", calls)
	}
}
