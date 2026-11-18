package server

// BUG: Поздние session RPC обходят owner fence; закрытые снимки занимают квоту.
// HYPOTHESIS: Проверка вне OpenChannel и общий счётчик active/closed TID оставляют гонки.
// SOLUTION: Транспорт проверяет точный lease под effect gate; guest считает только active TID.
// RESULT: FIXED — поздних автоматических RPC нет; пятый снимок и owner writer доступны.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

// Настоящий guest controller сохраняет закрытые TID до TTL для идемпотентности.
func TestReviewRepeatedReadonlySnapshotsDoNotExhaustGuestAttachments(t *testing.T) {
	d := newChildEngineServer(t)
	controller := guest.NewTerminalController(func(_ context.Context, step sessionctl.TerminalStep) relay.SessionResponse {
		body, _ := json.Marshal(sessionctl.TerminalResult{Proof: "current-session", Output: "snapshot content"})
		return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(body)}
	})
	defer controller.Close()
	childRelayWithSession(t, d, controller.Handle)
	location, status := mintChildView(t, d, "ro")
	if status != http.StatusSeeOther {
		t.Fatal(status)
	}
	for n := 1; n <= 5; n++ {
		r, err := http.Get(d.http.URL + location)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		t.Logf("readonly snapshot %d: status=%d body=%q", n, r.StatusCode, body)
		if r.StatusCode != http.StatusOK {
			t.Errorf("closed readonly snapshots exhausted the real guest attachment limit on refresh %d", n)
		}
	}
	r, body, _ := d.do(d.alice, "POST", "/api/commands/session_terminal", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.worker.Name, "action": "open", "mode": "rw"}})
	if r.StatusCode != http.StatusOK {
		t.Errorf("readonly snapshot refreshes also deny new owner terminal writer: status=%d body=%v", r.StatusCode, body)
	}
}

type reviewHeldReadConn struct {
	net.Conn
	resume <-chan struct{}
}

func (c reviewHeldReadConn) Read(b []byte) (int, error) {
	<-c.resume
	return c.Conn.Read(b)
}

type reviewHeldMessageHub struct {
	guestRelay
	sessionRelay
	opened chan struct{}
	resume <-chan struct{}
}

func (h reviewHeldMessageHub) DialMessage(ctx context.Context, uid string, generation int64, frame []byte) (net.Conn, error) {
	conn, err := h.guestRelay.DialMessage(ctx, uid, generation, frame)
	if err != nil {
		return nil, err
	}
	close(h.opened)
	return reviewHeldReadConn{Conn: conn, resume: h.resume}, nil
}

func TestReviewOwnerInterruptDoesNotWaitForMessageReceipt(t *testing.T) {
	d := newChildEngineServer(t)
	parent, _, received := childRelayWithSession(t, d, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		return relay.SessionResponse{OK: true, Outcome: "complete", Text: "owner interrupted"}
	})
	args := childGuestCreate(d, "receipt-pending")
	args["total"] = 1
	id := childResponseID(t, childCall(t, parent, "child_create", args))
	opened, resume := make(chan struct{}), make(chan struct{})
	d.srv.managed.hub = reviewHeldMessageHub{guestRelay: d.srv.managed.hub, sessionRelay: d.srv.managed.hub.(sessionRelay), opened: opened, resume: resume}
	done := make(chan struct{})
	go func() { defer close(done); d.srv.children.dispatch(context.Background()) }()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		close(resume)
		t.Fatal("message channel did not open")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := d.srv.executeOperatorSessionCommand(ctx, d.master.DelegatorID, d.swarm, "operator:receipt-pending", coordinationInvocation("session_terminal", map[string]any{"name": d.worker.Name, "action": "interrupt", "tid": "owner"}))
	close(resume)
	<-done
	if err != nil {
		t.Fatalf("owner interrupt was blocked by an already opened message awaiting its receipt: %v", err)
	}
	w, err := d.srv.store.SwarmChildWorkByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 || w.Task.Delivery != "unconfirmed" || w.Task.Outcome != "" || d.committed().State != "unconfirmed" {
		t.Fatalf("opened message evidence became success or cancellation: received=%d child=%+v parent=%+v", received.Load(), w.Task, d.committed())
	}
}

// Проверяем настоящий relay: автоматический RPC задержан перед открытием канала,
// затем отдельный owner slash подтверждён гостем и только потом RPC продолжен.
// В отличие от active rw attachment, slash не держит guest writer после ответа.
func TestReviewGuestMutationCannotOpenAfterOwnerIntervention(t *testing.T) {
	ts, master, worker := ownedWorkerSetup(t)
	d := newDispatchServerWithActors(t, ts, master, worker)
	var automatic atomic.Int32
	controller := guest.NewTerminalController(nil)
	defer controller.Close()
	parent, _, _ := childRelayWithSession(t, d, func(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
		return controller.Control(ctx, func() relay.SessionResponse {
			if req.Operation == "slash" && strings.HasPrefix(req.Caller, "actor:") {
				automatic.Add(1)
			}
			return relay.SessionResponse{OK: true, Outcome: "submitted", Text: "input submitted"}
		})
	})
	actual := d.srv.managed.hub.(sessionRelay)
	entered, resume := make(chan struct{}), make(chan struct{})
	d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(ctx context.Context, uid string, generation int64, req relay.SessionRequest) (relay.SessionResponse, error) {
		if req.Operation == "slash" && strings.HasPrefix(req.Caller, "actor:") {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return relay.SessionResponse{}, ctx.Err()
			}
		}
		return actual.CallSession(ctx, uid, generation, req)
	}}
	done := make(chan relay.SwarmResponse, 1)
	go func() {
		done <- childCall(t, parent, "slash", map[string]any{"name": d.worker.Name, "command": "/clear"})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(resume)
		t.Fatal("guest slash never reached its final relay boundary")
	}
	r, b, _ := d.do(d.alice, "POST", "/api/commands/slash", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.worker.Name, "command": "/status"}})
	expectStatus(t, "owner interrupt", r, b, http.StatusOK)
	close(resume)
	select {
	case response := <-done:
		t.Logf("guest response after owner interrupt: %+v", response)
	case <-time.After(3 * time.Second):
		t.Fatal("guest slash did not finish")
	}
	if n := automatic.Load(); n != 0 {
		t.Errorf("automatic slash reached the real relay guest after owner interrupt committed and completed: calls=%d", n)
	}
}

func TestReviewAttestationCannotOpenAfterOwnerIntervention(t *testing.T) {
	d := newDispatchServer(t)
	d.srv.steps.close()
	d.srv.steps = newStepDispatcher(d.srv)
	var automatic atomic.Int32
	controller := guest.NewTerminalController(nil)
	defer controller.Close()
	childRelayWithSession(t, d, func(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
		return controller.Control(ctx, func() relay.SessionResponse {
			if req.Operation == relay.WorkspaceAttestOperation {
				automatic.Add(1)
				return relay.SessionResponse{Outcome: "failed", Text: "attestation effect reached guest"}
			}
			return relay.SessionResponse{OK: true, Outcome: "submitted", Text: "owner input submitted"}
		})
	})
	actual := d.srv.managed.hub.(sessionRelay)
	entered, resume := make(chan struct{}), make(chan struct{})
	d.srv.managed.hub = commandSessionHub{guestRelay: d.srv.managed.hub, call: func(ctx context.Context, uid string, generation int64, req relay.SessionRequest) (relay.SessionResponse, error) {
		if req.Operation == relay.WorkspaceAttestOperation {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return relay.SessionResponse{}, ctx.Err()
			}
		}
		return actual.CallSession(ctx, uid, generation, req)
	}}
	r, b, _ := d.do(d.alice, "POST", "/api/swarms/"+d.swarm+"/start", d.csrf(d.alice), map[string]any{"definition": "deliver", "commit": dispatchCommit, "goal": dispatchGoal})
	expectStatus(t, "start", r, b, http.StatusOK)
	done := make(chan struct{})
	go func() { defer close(done); d.srv.steps.dispatch(context.Background(), d.swarm) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(resume)
		t.Fatal("attestation never reached its final relay boundary")
	}
	r, b, _ = d.do(d.alice, "POST", "/api/commands/slash", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.worker.Name, "command": "/status"}})
	expectStatus(t, "owner interrupt", r, b, http.StatusOK)
	if got := d.committed().State; got != "unconfirmed" {
		t.Errorf("owner interruption did not commit: %s", got)
	}
	close(resume)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("attestation did not finish")
	}
	if n := automatic.Load(); n != 0 {
		t.Errorf("automatic workspace_attest reached the real relay guest after owner interrupt committed and completed: calls=%d", n)
	}
}
