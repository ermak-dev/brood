package server

import (
	"context"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// BUG: Namespace preflight can outlive the operator's authority.
// SPEC: Reauthorize immediately before a session side effect, not only before preflight or after it executes.
func TestOperatorSessionReauthorizesAfterPreflight(t *testing.T) {
	ts, _, master, _ := masterSetup(t)
	ctx := context.Background()
	called := false
	ts.srv.managed.hub = commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		called = true
		return relay.SessionResponse{OK: true, Text: "changed", Outcome: "complete"}, nil
	}}
	ts.fake.beforeNamespaceGet = func() {
		if err := ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, true); err != nil {
			t.Fatal(err)
		}
	}
	inv := coordinationInvocation("slash", map[string]any{"name": "master", "command": "/status"})
	if _, err := ts.srv.executeOperatorSessionCommand(ctx, master.DelegatorID, master.SwarmID, "user:test", inv); err == nil || called {
		t.Fatalf("mutation after revocation: %v called=%v", err, called)
	}
}

type reservingSessionHub struct {
	commandSessionHub
	onConnect func()
}

func (h reservingSessionHub) Connected(string, int64) bool { h.onConnect(); return true }

func TestControlLeaseRecheckedAfterConnectivity(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	ctx := context.Background()
	if err := ts.srv.store.CreateUser(ctx, store.User{ID: "other-admin", Username: "other-admin", PasswordHash: "test-only", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	claim := coordinationClaim(master, sw, "new-owner")
	claim.Target = store.CoordinationActor{UID: worker.UID, Generation: worker.Incarnation}
	called := false
	hub := commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		called = true
		return relay.SessionResponse{OK: true, Text: "changed", Outcome: "complete"}, nil
	}}
	ts.srv.managed.hub = reservingSessionHub{commandSessionHub: hub, onConnect: func() {
		if _, err := ts.srv.store.ClaimDispatchLease(ctx, claim); err != nil {
			t.Fatal(err)
		}
	}}
	inv := coordinationInvocation("slash", map[string]any{"name": "worker", "command": "/status"})
	if _, err := ts.srv.executeOperatorSessionCommand(ctx, "other-admin", master.SwarmID, "user:other-admin", inv); err != nil || !called {
		t.Fatalf("explicit administrator control denied after connectivity: %v called=%v", err, called)
	}
	if held, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, worker.UID); err != nil || held != claim {
		t.Fatalf("owner intervention lost automatic task reservation: %+v %v", held, err)
	}
}

func TestWorkspaceReservationBlocksDispatchBeforeRPC(t *testing.T) {
	ts, master, worker := ownedWorkerSetup(t)
	ctx := context.Background()
	sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	targetClaim := coordinationClaim(master, sw, "racing-task")
	targetClaim.Target = store.CoordinationActor{UID: worker.UID, Generation: worker.Incarnation}
	blocked := false
	called := false
	hub := commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		called = true
		return relay.SessionResponse{OK: true, Text: "restarted", Outcome: "complete"}, nil
	}}
	ts.srv.managed.hub = reservingSessionHub{commandSessionHub: hub, onConnect: func() { _, err := ts.srv.store.ClaimDispatchLease(ctx, targetClaim); blocked = err != nil }}
	inv := coordinationInvocation("restart", map[string]any{"name": "worker", "confirm": true})
	if _, err := ts.srv.managed.executeGuestSessionCommand(ctx, master, inv); err != nil || !called || !blocked {
		t.Fatalf("reservation missing: %v called=%v blocked=%v", err, called, blocked)
	}
}
