package server

// BUG: Automatic child reservations deny authorized owner session control.
// HYPOTHESIS: Human and automatic session actions share the child-denial gate.

import (
	"context"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func childSessionMutations(name string) []struct {
	tool string
	args map[string]any
} {
	return []struct {
		tool string
		args map[string]any
	}{
		{"slash", map[string]any{"name": name, "command": "/clear"}},
		{"session_terminal", map[string]any{"name": name, "action": "input", "tid": "terminal", "seq": 1, "data": "interrupt work"}},
		{"session_terminal", map[string]any{"name": name, "action": "interrupt", "tid": "terminal"}},
		{"session_terminal", map[string]any{"name": name, "action": "resize", "tid": "terminal", "cols": 100, "rows": 30}},
		{"session_terminal", map[string]any{"name": name, "action": "open", "mode": "rw"}},
	}
}

func TestChildSessionOwnerMutationPreservesEvidence(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"pending", "running", "unconfirmed"} {
		t.Run(state, func(t *testing.T) {
			d := newChildEngineServer(t)
			c, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "one"))
			if err != nil {
				t.Fatal(err)
			}
			if state != "pending" {
				if state == "unconfirmed" {
					d.mu.Lock()
					d.deliver = func(deliveredMessage) (relay.MessageResponse, error) {
						return relay.MessageResponse{Verdict: "submitted without receipt"}, nil
					}
					d.mu.Unlock()
				}
				d.srv.children.dispatch(ctx)
			}
			var calls atomic.Int32
			d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(ctx context.Context, _ string, _ int64, _ relay.SessionRequest) (relay.SessionResponse, error) {
				calls.Add(1)
				if step := d.committed(); step.State != "unconfirmed" {
					t.Errorf("effect preceded durable interruption: %+v", step)
				}
				return relay.SessionResponse{OK: true, Outcome: "completed", Text: "applied"}, nil
			}}
			for _, target := range []store.ManagedActor{d.master, d.worker} {
				for _, inv := range childSessionMutations(target.Name) {
					r, b, _ := d.do(d.alice, "POST", "/api/commands/"+inv.tool, d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": inv.args})
					expectStatus(t, "owner intervention with "+state+" child", r, b, http.StatusOK)
				}
			}
			if calls.Load() != 10 {
				t.Fatalf("owner controls did not reach relay: %d", calls.Load())
			}
			w, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if state == "pending" && w.Task.State != "pending" || state != "pending" && w.Task.State != "running" {
				t.Fatalf("intervention orphaned child: %+v", w.Task)
			}
			_, before := d.calls()
			d.srv.children.dispatch(ctx)
			_, after := d.calls()
			if len(after) != len(before) {
				t.Fatal("interrupted child automatically delivered again")
			}
		})
	}
}

func TestChildSessionReadonlyDoesNotInterrupt(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	c, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "readonly"))
	if err != nil {
		t.Fatal(err)
	}
	d.srv.children.dispatch(ctx)
	before, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := d.committed()
	d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		return relay.SessionResponse{OK: true, Outcome: "completed", Text: "snapshot"}, nil
	}}
	for _, args := range []map[string]any{{"name": d.worker.Name, "action": "open", "mode": "ro"}, {"name": d.worker.Name, "action": "read", "tid": "readonly"}, {"name": d.master.Name, "action": "read", "tid": "readonly"}} {
		r, b, _ := d.do(d.alice, "POST", "/api/commands/session_terminal", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": args})
		expectStatus(t, "read-only child session", r, b, http.StatusOK)
	}
	after, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(parent, d.committed()) {
		t.Fatal("read-only inspection changed execution evidence")
	}
}

func TestChildSessionOwnerLateHoldRecordsBeforeEffect(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	var calls atomic.Int32
	d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		calls.Add(1)
		if d.committed().State != "unconfirmed" {
			t.Error("late hold not interrupted before effect")
		}
		return relay.SessionResponse{OK: true, Outcome: "completed"}, nil
	}}
	var once atomic.Bool
	d.onConnected(func(base commandSessionHub, uid string, generation int64) bool {
		if uid == d.worker.UID && once.CompareAndSwap(false, true) {
			if _, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "late")); err != nil {
				t.Error(err)
			}
		}
		return base.Connected(uid, generation)
	})
	r, b, _ := d.do(d.alice, "POST", "/api/commands/session_terminal", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.worker.Name, "action": "interrupt", "tid": "terminal"}})
	expectStatus(t, "owner late child intervention", r, b, http.StatusOK)
	if calls.Load() != 1 || !once.Load() {
		t.Fatalf("late child incorrectly denied explicit owner control: %+v calls=%d", r, calls.Load())
	}
}
