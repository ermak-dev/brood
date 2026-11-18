package server

// BUG: Вмешательство запрещало поздние отчёты, но не отсекало уже проверенную отправку.
// HYPOTHESIS: Scheduling-ограждения применялись к evidence; проверка отправки не была связана с открытием relay.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

func TestReviewOwnerParentUncertaintyRetainsLateChildEvidence(t *testing.T) {
	d := newChildEngineServer(t)
	parent, worker, _ := childRelayWithSession(t, d, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		return relay.SessionResponse{Outcome: "unconfirmed", Text: "owner input may have applied"}
	})
	args := childGuestCreate(d, "late-parent-owner")
	args["total"] = 1
	id := childResponseID(t, childCall(t, parent, "child_create", args))
	d.srv.children.dispatch(context.Background())
	r, b, _ := d.do(d.alice, "POST", "/api/commands/session_terminal", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.master.Name, "action": "interrupt", "tid": "owner"}})
	expectStatus(t, "owner input with unknown relay outcome", r, b, http.StatusConflict)
	t.Logf("owner parent intervention: status=%d body=%v", r.StatusCode, b)
	if response := childCall(t, worker, "child_complete", map[string]any{"child_id": id, "request_id": "late-parent-owner", "outcome": "success", "report": "late independent evidence"}); !response.OK {
		t.Errorf("late child evidence rejected after owner intervention: %+v", response)
	}
	if response := childCall(t, parent, "child_get", map[string]any{"child_id": id}); !response.OK {
		t.Errorf("retained child history cannot be inspected: %+v", response)
	}
	response := childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": args["parent_request_id"], "wait_seconds": 0})
	if response.Outcome != "unconfirmed" {
		t.Errorf("expected truthful interrupted join, got %+v", response)
	}
	w, err := d.srv.store.SwarmChildWorkByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if w.Task.Report != "late independent evidence" {
		t.Errorf("late report missing from SQLite: %+v", w.Task)
	}
}

func TestReviewOwnerCommitBetweenFinalCheckAndRelaySend(t *testing.T) {
	d := newChildEngineServer(t)
	parent, _, received := childRelayWithSession(t, d, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		return relay.SessionResponse{OK: true, Outcome: "completed", Text: "owner interruption applied"}
	})
	args := childGuestCreate(d, "send-race")
	args["total"] = 1
	id := childResponseID(t, childCall(t, parent, "child_create", args))
	beforeRelay := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan struct{})
	actual := d.srv.managed.deliverMessage
	d.srv.managed.deliverMessage = func(ctx context.Context, uid string, generation int64, request relay.MessageRequest) (relay.MessageResponse, error) {
		close(beforeRelay)
		select {
		case <-resume:
		case <-ctx.Done():
			return relay.MessageResponse{}, ctx.Err()
		}
		return actual(ctx, uid, generation, request)
	}
	go func() { defer close(done); d.srv.children.dispatch(context.Background()) }()
	select {
	case <-beforeRelay:
	case <-time.After(3 * time.Second):
		close(resume)
		t.Fatal("automatic delivery never reached final send boundary")
	}
	r, b, _ := d.do(d.alice, "POST", "/api/commands/session_terminal", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.worker.Name, "action": "interrupt", "tid": "owner"}})
	expectStatus(t, "concurrent owner intervention", r, b, http.StatusOK)
	if d.committed().State != "unconfirmed" {
		t.Error("owner interruption not committed")
	}
	close(resume)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic dispatch did not stop")
	}
	w, err := d.srv.store.SwarmChildWorkByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("post-owner child delivery=%s state=%s received=%d", w.Task.Delivery, w.Task.State, received.Load())
	if received.Load() != 0 {
		t.Error("automatic instruction reached actual relay guest after owner interruption committed and applied")
	}
}
