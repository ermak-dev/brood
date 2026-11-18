package server

// BUG: Unknown owner input must retain evidence without denying further owner control.
// HYPOTHESIS: A single automatic workspace slot also constrained explicit intervention.
// SOLUTION: Independent owner operations retain uncertainty without automatic retries.

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func TestSessionMutationUnknownRetainsReservation(t *testing.T) {
	for _, mutation := range childSessionMutations("worker") {
		for _, outcome := range []string{"cancelled", "unknown", "unconfirmed"} {
			t.Run(mutation.tool+"/"+func() string { v, _ := mutation.args["action"].(string); return v }()+"/"+outcome, func(t *testing.T) {
				d := newChildEngineServer(t)
				mutation.args["name"] = d.worker.Name
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
					calls++
					if outcome == "cancelled" {
						cancel()
						return relay.SessionResponse{}, context.Canceled
					}
					return relay.SessionResponse{Outcome: outcome}, nil
				}}
				_, err := d.srv.executeOperatorSessionCommand(ctx, d.master.DelegatorID, d.swarm, "operator:unknown", coordinationInvocation(mutation.tool, mutation.args))
				if err == nil || calls != 1 {
					t.Fatalf("unknown invocation: calls=%d err=%v", calls, err)
				}
				op, err := d.srv.store.CurrentActorOperation(context.Background(), d.worker.UID)
				if err != nil || op.State != "unconfirmed" {
					t.Fatalf("unknown effect released: %+v %v", op, err)
				}
				if _, err = d.srv.managed.createChild(context.Background(), d.master, childArgs(d, "unknown-child")); err == nil {
					t.Fatal("unknown effect admitted child")
				}
				// This is a new explicit owner action, not an automatic retry of the first.
				if _, err = d.srv.executeOperatorSessionCommand(context.Background(), d.master.DelegatorID, d.swarm, "operator:new-intervention", coordinationInvocation(mutation.tool, mutation.args)); err == nil || calls != 2 {
					t.Fatalf("new owner action denied or uncertainty hidden: %v calls=%d", err, calls)
				}
				d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
					return relay.SessionResponse{OK: true, Outcome: "completed", Text: "inspection"}, nil
				}}
				if _, err = d.srv.executeOperatorSessionCommand(context.Background(), d.master.DelegatorID, d.swarm, "operator:inspect", coordinationInvocation("session_terminal", map[string]any{"name": d.worker.Name, "action": "read", "tid": "ro"})); err != nil {
					t.Fatalf("unknown blocked readonly: %v", err)
				}
				release := coordinationInvocation("dispatch", map[string]any{"to": d.worker.Name, "action": "release", "request_id": op.OperationID})
				if _, err = d.srv.executeOperatorCoordinationCommand(context.Background(), d.master.DelegatorID, d.swarm, release); err == nil {
					t.Fatal("dispatch release forgot uncertain owner intervention")
				}
				if retained, err := d.srv.store.CurrentActorOperation(context.Background(), d.worker.UID); err != nil || retained != op {
					t.Fatalf("uncertain owner evidence changed: %+v %v", retained, err)
				}
				if _, err = d.srv.store.AcquireActorOperation(context.Background(), d.worker.UID, d.worker.Incarnation, "automatic", "automatic", "restart"); !errors.Is(err, store.ErrCoordinationConflict) {
					t.Fatalf("automatic mutation crossed uncertain owner effects: %v", err)
				}
			})
		}
	}
}
