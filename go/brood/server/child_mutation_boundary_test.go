package server

// BUG: Последняя авторизация session mutation не исключает child dispatch.
// HYPOTHESIS: Между проверкой hold и внешним эффектом нет durable reservation.
// SOLUTION: Перед RPC фиксируем workspace slot с локальной проверкой authority.
// RESULT: FIXED — пересечение не доставляет child, unknown удерживает reservation.
// STATUS: FIXED

import (
	"context"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestChildSessionFinalBoundaryReservation(t *testing.T) {
	for _, mutation := range childSessionMutations("worker") {
		t.Run(mutation.tool+"/"+func() string { a, _ := mutation.args["action"].(string); return a }(), func(t *testing.T) {
			d := newChildEngineServer(t)
			mutation.args["name"] = d.worker.Name
			var calls atomic.Int32
			d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
				calls.Add(1)
				return relay.SessionResponse{OK: true, Outcome: "completed"}, nil
			}}
			checks := 0
			accepted := false
			ctx := context.WithValue(context.Background(), commandSessionAuthorityKey{}, func(ctx context.Context) bool {
				checks++
				if checks == 2 {
					_, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "last-auth"))
					accepted = err == nil
					d.srv.children.dispatch(ctx)
				}
				return true
			})
			_, _ = d.srv.executeOperatorSessionCommand(ctx, d.master.DelegatorID, d.swarm, "operator:boundary", coordinationInvocation(mutation.tool, mutation.args))
			if checks < 2 {
				t.Fatal("final authorization was not exercised")
			}
			if calls.Load() != 1 {
				t.Fatalf("explicit owner intervention was denied: RPCs=%d", calls.Load())
			}
			if accepted && d.committed().State != "unconfirmed" {
				t.Fatal("owner effect failed to retain late child interruption evidence")
			}
		})
	}
}

func TestChildSessionAuthenticatedOverlapReservation(t *testing.T) {
	d := newChildEngineServer(t)
	var parent relay.SwarmCaller
	scheduled := make(chan relay.SwarmResponse, 1)
	parent, _, received := childRelayWithSession(t, d, func(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
		scheduled <- childCall(t, parent, "child_create", childGuestCreate(d, "during-slash"))
		d.srv.children.dispatch(ctx)
		return relay.SessionResponse{OK: true, Outcome: "completed", Text: "applied"}
	})
	response, _, _ := d.do(d.alice, "POST", "/api/commands/slash", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.worker.Name, "command": "/clear"}})
	child := <-scheduled
	if child.OK || received.Load() != 0 {
		t.Fatalf("mutating session admitted a child: HTTP=%d child=%+v deliveries=%d", response.StatusCode, child, received.Load())
	}
}
