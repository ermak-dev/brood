package server

import (
	"context"
	"encoding/json"
	"errors"

	"brood.dev/go/brood/store"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

type commandSessionHub struct {
	guestRelay
	call func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error)
}

func (h commandSessionHub) CallSession(ctx context.Context, uid string, g int64, r relay.SessionRequest) (relay.SessionResponse, error) {
	return h.call(ctx, uid, g, r)
}

// FEATURE: Session commands share actor authorization and incarnation fencing.
// SPEC: The server supplies caller identity and suppresses private results after revocation.
func TestGuestSessionCommandRechecksDelegation(t *testing.T) {
	ts, _, master, _ := masterSetup(t)
	called := false
	ts.srv.managed.hub = commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(ctx context.Context, uid string, g int64, r relay.SessionRequest) (relay.SessionResponse, error) {
		called = true
		if uid != master.UID || g != master.Incarnation || !strings.HasPrefix(r.Caller, "actor:") {
			t.Fatalf("unbound request: %s %d %+v", uid, g, r)
		}
		if err := ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, true); err != nil {
			t.Fatal(err)
		}
		return relay.SessionResponse{OK: true, Text: "private-session-sentinel", Outcome: "complete"}, nil
	}}
	got := ts.srv.managed.handleSwarmRequest(context.Background(), master.UID, master.Incarnation, relay.SwarmRequest{Tool: "slash", Arguments: json.RawMessage(`{"name":"master","command":"/status"}`)})
	if !called {
		t.Fatal("session service was not called")
	}
	if got.OK || strings.Contains(got.Text, "private-session-sentinel") {
		t.Fatalf("revoked response escaped: %+v", got)
	}
}

func TestWorkspaceMutationRequiresDispatchRelease(t *testing.T) {
	ts, master, _ := coordinationServerSetup(t)
	ctx := context.Background()
	ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		return relay.MessageResponse{OK: true}, nil
	}
	if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "held"})); err != nil {
		t.Fatal(err)
	}
	called := false
	ts.srv.managed.hub = commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		called = true
		return relay.SessionResponse{OK: true, Text: "changed", Outcome: "complete"}, nil
	}}
	inv := coordinationInvocation("restart", map[string]any{"name": "worker", "confirm": true})
	if _, err := ts.srv.managed.executeGuestSessionCommand(ctx, master, inv); err == nil || called {
		t.Fatalf("active lease permitted restart: %v called=%v", err, called)
	}
}

func TestUnknownWorkspaceOperationRetainsExactRecovery(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	ctx := context.Background()
	ts.srv.managed.hub = commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		return relay.SessionResponse{Text: "partial effect", Outcome: "unknown"}, nil
	}}
	inv := coordinationInvocation("restart", map[string]any{"name": "worker", "confirm": true})
	if _, err := ts.srv.managed.executeGuestSessionCommand(ctx, master, inv); err == nil {
		t.Fatal("unknown restart returned success")
	}
	op, err := ts.srv.store.CurrentActorOperation(ctx, worker.UID)
	if err != nil || op.State != "unconfirmed" {
		t.Fatalf("unknown operation was unlocked: %+v %v", op, err)
	}
	release := coordinationInvocation("dispatch", map[string]any{"to": "worker", "action": "release", "request_id": op.OperationID})
	if _, err := ts.srv.executeOperatorCoordinationCommand(ctx, master.DelegatorID, master.SwarmID, release); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.srv.store.CurrentActorOperation(ctx, worker.UID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("exact recovery failed: %v", err)
	}
}

func TestSessionTargetUsesDurableRole(t *testing.T) {
	ts, _, master, _ := masterSetup(t)
	ctx := context.Background()
	worker := master
	worker.Role = "worker"
	target, err := ts.srv.managed.authorizeCommandTarget(ctx, worker, master.Name, false)
	if err != nil || target.UID != master.UID {
		t.Fatalf("durable role resolution: %+v %v", target, err)
	}
}
