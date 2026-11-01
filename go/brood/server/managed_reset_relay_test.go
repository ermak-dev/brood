package server

import (
	"context"
	"testing"
)

// BUG: A terminally failed reset cleanup re-authorized the old guest relay.
// SPEC: Only a start operation authorizes a guest; a reset intent never does,
// including after its failed stage settles to done with the old incarnation.
func TestFailedResetCleanupKeepsOldGuestRelayRevoked(t *testing.T) {
	ts, alice, _, id, _, hub := managedSetup(t)
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "reset-relay"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	hub.connected = true
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	ctx := context.Background()
	a := mustManagedActor(t, ts, id)
	secret := ts.srv.managed.registry.relaySecret
	token := guestToken(secret, a.UID, a.Incarnation)
	if a.Phase != "running" || !ts.srv.managed.authorizeGuest(a.UID, a.Incarnation, token) {
		t.Fatalf("running start operation must authorize its guest: %+v", a)
	}
	a, err := ts.srv.store.BeginManagedActorOperation(ctx, a.UID, a.Revision, "reset-relay-op", "reset")
	if err != nil {
		t.Fatal(err)
	}
	if ts.srv.managed.authorizeGuest(a.UID, a.Incarnation, token) {
		t.Fatal("reset cleanup stage authorized old guest")
	}
	a, err = ts.srv.store.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "UPID:failed-stop")
	if err != nil {
		t.Fatal(err)
	}
	a, err = ts.srv.store.ResolveManagedActorTaskFailure(ctx, a.UID, a.Revision, a.OperationID, "stop", "UPID:failed-stop", "running")
	if err != nil {
		t.Fatal(err)
	}
	if a.Stage != "done" || a.Operation != "reset" || a.Desired != "running" {
		t.Fatalf("unexpected failed reset state: %+v", a)
	}
	if ts.srv.managed.authorizeGuest(a.UID, a.Incarnation, token) {
		t.Fatal("failed reset cleanup re-authorized the old guest relay")
	}
}
