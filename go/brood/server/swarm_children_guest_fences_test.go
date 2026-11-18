package server

// BUG: Workspace и session mutations обходят child hold, completion скрывает join.
// HYPOTHESIS: Ошибки общей authority видны через настоящие TLS/WSS/SSH команды.
// SOLUTION: Транзакционный workspace gate, общий session gate и typed join refusal.
// RESULT: FIXED — реальные relay RPC исключены для mutations, readonly разрешён.
// STATUS: FIXED

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func TestChildGuestParentReservationAuthenticatedRelay(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	parent, _, received := childRelay(t, d)
	if _, err := d.srv.store.AcquireActorOperation(ctx, d.master.UID, d.master.Incarnation, "parent-restart", "operator:test", "restart"); err != nil {
		t.Fatal(err)
	}
	r := childCall(t, parent, "child_create", childGuestCreate(d, "one"))
	d.srv.children.dispatch(ctx)
	if r.OK || r.Outcome != "refused" || received.Load() != 0 {
		t.Fatalf("reserved parent scheduled through authenticated relay: %+v deliveries=%d", r, received.Load())
	}
}

func TestChildGuestEnqueueDuringHTTPParentRestart(t *testing.T) {
	d := newChildEngineServer(t)
	var parent relay.SwarmCaller
	scheduled := make(chan relay.SwarmResponse, 1)
	parent, _, received := childRelayWithSession(t, d, func(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
		if req.Operation != "restart" {
			t.Errorf("unexpected session RPC: %s", req.Operation)
			return relay.SessionResponse{Outcome: "refused"}
		}
		// HTTP restart уже получил workspace reservation, но ещё не завершил RPC.
		// Вложенная настоящая guest-команда видит именно committed SQL intent.
		scheduled <- childCall(t, parent, "child_create", childGuestCreate(d, "during-http-restart"))
		d.srv.children.dispatch(ctx)
		return relay.SessionResponse{OK: true, Outcome: "completed", Text: "restart confirmed"}
	})
	r, b, _ := d.do(d.alice, "POST", "/api/commands/restart", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.master.Name, "confirm": true}})
	expectStatus(t, "parent restart", r, b, http.StatusOK)
	child := <-scheduled
	if child.OK || child.Outcome != "refused" || received.Load() != 0 {
		t.Fatalf("child dispatched during HTTP parent restart: %+v deliveries=%d", child, received.Load())
	}
	if _, err := d.srv.store.CurrentActorOperation(context.Background(), d.master.UID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("confirmed HTTP restart retained its reservation: %v", err)
	}
}

func TestChildGuestGenericCompletionAuthenticatedRelay(t *testing.T) {
	d := newChildEngineServer(t)
	parent, _, received := childRelay(t, d)
	childResponseID(t, childCall(t, parent, "child_create", childGuestCreate(d, "one")))
	d.srv.children.dispatch(context.Background())
	r := childCall(t, parent, "pipeline_visit_complete", map[string]any{"run_id": d.swarm, "sequence": 1, "request_id": d.committed().Request, "outcome": "success"})
	if r.OK || r.Outcome != "refused" || !strings.Contains(r.Text, "child_join") || strings.Contains(r.Text, "swarm child conflict") || d.committed().State != "inflight" || received.Load() != 1 {
		t.Fatalf("generic completion did not publicly preserve join gate: %+v", r)
	}
}

func newOwnedChildEngineServer(t *testing.T) *dispatchServer {
	t.Helper()
	ts, master, worker := ownedWorkerSetup(t)
	d := newDispatchServerWithActors(t, ts, master, worker)
	d.srv.children.close()
	d.srv.children = newChildDispatcher(d.srv)
	d.bind("build", master.UID)
	d.bind("review", worker.UID)
	d.start("deliver")
	return d
}

func TestChildGuestSessionMutationAuthenticatedRelay(t *testing.T) {
	ctx := context.Background()
	d := newOwnedChildEngineServer(t)
	var calls atomic.Int32
	parent, worker, received := childRelayWithSession(t, d, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		calls.Add(1)
		return relay.SessionResponse{OK: true, Outcome: "completed", Text: "independent guest snapshot"}
	})
	args := childGuestCreate(d, "one")
	args["total"] = 1
	id := childResponseID(t, childCall(t, parent, "child_create", args))
	d.srv.children.dispatch(ctx)
	for _, inv := range childSessionMutations(d.worker.Name) {
		r := childCall(t, parent, inv.tool, inv.args)
		if r.OK || !strings.Contains(r.Text, "child") || calls.Load() != 0 {
			t.Fatalf("running child did not block guest %s: %+v sessionRPCs=%d", inv.tool, r, calls.Load())
		}
	}
	for _, source := range []relay.SwarmCaller{parent, worker} {
		r := childCall(t, source, "session_terminal", map[string]any{"name": d.worker.Name, "action": "open", "mode": "ro"})
		if !r.OK || r.Text != "independent guest snapshot" {
			t.Fatalf("read-only inspection failed: %+v", r)
		}
	}
	if calls.Load() != 2 || received.Load() != 1 {
		t.Fatalf("wrong RPC counts: session=%d child=%d", calls.Load(), received.Load())
	}
	w, err := d.srv.store.SwarmChildWorkByID(ctx, id)
	if err != nil || w.Task.State != "running" {
		t.Fatalf("session command changed child: %+v %v", w.Task, err)
	}
	if r := childCall(t, worker, "child_complete", map[string]any{"child_id": id, "request_id": "one", "outcome": "success", "report": "Independent completed task"}); !r.OK {
		t.Fatalf("complete: %+v", r)
	}
	// Отчёт освобождает executor; родитель ещё не делает join и не запускает
	// следующий graph step на том же worker, чтобы не смешивать виды RPC.
	r := childCall(t, parent, "slash", map[string]any{"name": d.worker.Name, "command": "/clear"})
	if !r.OK || calls.Load() != 3 {
		t.Fatalf("completed child retained session hold: %+v RPCs=%d", r, calls.Load())
	}
}
