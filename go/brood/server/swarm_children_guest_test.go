package server

// FEATURE: Authenticated guest child commands over the real relay (#289).
// SPEC: Real TLS/WSS/SSH principals drive SQLite-backed child create/status/join
// and independent completion. Receipt is not completion, retries never resend,
// and parent/other executor/old generation cannot forge child or graph outcomes.
// IMPLEMENTATION: Catalog adapters, bounded join and scoped report/status views.
// RESULT: IMPLEMENTED — real authenticated relay, SQLite, fake provider,
// full Go suite and focused race checks pass; no live PVE or model acceptance.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func childRelay(t *testing.T, d *dispatchServer, receipts ...relay.MessageResponse) (relay.SwarmCaller, relay.SwarmCaller, *atomic.Int32) {
	t.Helper()
	return childRelayWithSession(t, d, nil, receipts...)
}

func childRelayWithSession(t *testing.T, d *dispatchServer, session func(context.Context, relay.SessionRequest) relay.SessionResponse, receipts ...relay.MessageResponse) (relay.SwarmCaller, relay.SwarmCaller, *atomic.Int32) {
	t.Helper()
	m := d.srv.managed
	hub, err := relay.NewHub([]byte(m.registry.relaySecret), m.authorizeGuest)
	if err != nil {
		t.Fatal(err)
	}
	m.hub = hub
	m.deliverMessage = m.deliverViaHub
	m.enableSwarmMessages()
	server := httptest.NewTLSServer(hub.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	calls := make(chan struct {
		uid  string
		call relay.SwarmCaller
	}, 2)
	received := new(atomic.Int32)
	for _, a := range []store.ManagedActor{d.master, d.worker} {
		cfg := relay.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https"), ActorUID: a.UID, Generation: a.Incarnation, Token: guestToken(m.registry.relaySecret, a.UID, a.Incarnation), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), HostKey: hub.HostKey(), DialTarget: func(context.Context) (net.Conn, error) { return nil, errors.New("terminal unused") }, Session: session, OnSwarmSession: func(ctx context.Context, call relay.SwarmCaller) {
			calls <- struct {
				uid  string
				call relay.SwarmCaller
			}{a.UID, call}
			<-ctx.Done()
		}, HandleMessage: func(_ context.Context, request relay.MessageRequest) relay.MessageResponse {
			received.Add(1)
			if a.UID != d.worker.UID || !strings.Contains(request.Body, "child_complete") {
				t.Errorf("wrong child delivery: %s %+v", a.UID, request)
			}
			// Independent SQLite connection proves the claim committed before the effect.
			db, err := sql.Open("sqlite", d.dbPath)
			if err != nil {
				t.Error(err)
				return relay.MessageResponse{}
			}
			defer db.Close()
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM swarm_child_tasks WHERE actor_uid=? AND state='running' AND delivery='sending'`, a.UID).Scan(&n); err != nil || n != 1 {
				t.Errorf("uncommitted/nonexclusive child allocation: %d %v", n, err)
			}
			if len(receipts) != 0 {
				return receipts[0]
			}
			return relay.MessageResponse{OK: true, Verdict: "delivery receipt confirmed"}
		}}
		go func() { done <- relay.RunClient(ctx, cfg) }()
	}
	t.Cleanup(func() {
		cancel()
		hub.Close()
		server.Close()
		for range 2 {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("relay client did not stop")
			}
		}
	})
	byUID := map[string]relay.SwarmCaller{}
	for range 2 {
		select {
		case c := <-calls:
			byUID[c.uid] = c.call
		case <-time.After(3 * time.Second):
			t.Fatal("guest did not authenticate")
		}
	}
	if byUID[d.master.UID] == nil || byUID[d.worker.UID] == nil {
		t.Fatal("expected one authenticated relay client for each fixture actor")
	}
	// Client authentication can finish before the hub registers the peer.
	ready := func() bool {
		return hub.Connected(d.master.UID, d.master.Incarnation) && hub.Connected(d.worker.UID, d.worker.Incarnation)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ready() {
		t.Fatal("relay hub did not register both fixture actors")
	}
	return byUID[d.master.UID], byUID[d.worker.UID], received
}

func childCall(t *testing.T, call relay.SwarmCaller, name string, args map[string]any) relay.SwarmResponse {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return call(ctx, relay.SwarmRequest{Tool: name, Arguments: raw})
}
func childResponseID(t *testing.T, r relay.SwarmResponse) string {
	t.Helper()
	if !r.OK {
		t.Fatalf("child command refused: %+v", r)
	}
	var data struct {
		ID string `json:"child_id"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil || data.ID == "" {
		t.Fatalf("child result: %+v %v", r, err)
	}
	return data.ID
}
func childGuestCreate(d *dispatchServer, key string) map[string]any {
	return map[string]any{"parent_sequence": 1, "parent_request_id": d.committed().Request, "request_id": key, "role": "review", "task": "Implement " + key, "mode": "all", "total": 2}
}

func TestChildGuestAuthenticatedRelayRoundTrip(t *testing.T) {
	d := newChildEngineServer(t)
	parent, worker, received := childRelay(t, d)
	oneArgs := childGuestCreate(d, "one")
	one := childResponseID(t, childCall(t, parent, "child_create", oneArgs))
	two := childResponseID(t, childCall(t, parent, "child_create", childGuestCreate(d, "two")))
	if again := childResponseID(t, childCall(t, parent, "child_create", oneArgs)); again != one {
		t.Fatal("retry created another child")
	}
	d.srv.children.dispatch(context.Background())
	if received.Load() != 1 {
		t.Fatalf("same worker received concurrent children: %d", received.Load())
	}
	if r := childCall(t, parent, "child_complete", map[string]any{"child_id": one, "request_id": "one", "outcome": "success", "report": "forged"}); r.OK {
		t.Fatal("parent forged executor report")
	}
	if r := childCall(t, worker, "pipeline_visit_complete", map[string]any{"run_id": d.swarm, "sequence": 1, "request_id": d.committed().Request, "outcome": "success"}); r.OK {
		t.Fatal("child forged graph completion")
	}
	if r := childCall(t, worker, "child_complete", map[string]any{"child_id": one, "request_id": "one", "outcome": "success", "report": "First report"}); !r.OK {
		t.Fatalf("complete: %+v", r)
	}
	join := map[string]any{"parent_sequence": 1, "parent_request_id": oneArgs["parent_request_id"], "wait_seconds": 0}
	r := childCall(t, parent, "child_join", join)
	if !r.OK || r.Outcome != "accepted" || d.committed().State == "completed" {
		t.Fatalf("early join: %+v", r)
	}
	d.srv.children.dispatch(context.Background())
	if r := childCall(t, worker, "child_complete", map[string]any{"child_id": two, "request_id": "two", "outcome": "fail", "report": "Second failure"}); !r.OK {
		t.Fatalf("complete: %+v", r)
	}
	if r := childCall(t, parent, "child_join", join); !r.OK || r.Outcome != "completed" {
		t.Fatalf("join: %+v", r)
	}
	v, err := d.srv.store.GetSwarmStep(context.Background(), d.swarm, 1)
	if err != nil || v.Outcome == nil || *v.Outcome != "fail" || !strings.Contains(v.Report, "Second failure") {
		t.Fatalf("parent: %+v %v", v, err)
	}
	if received.Load() != 2 {
		t.Fatalf("duplicate sends: %d", received.Load())
	}
}

func TestChildGuestIdentityAndStatusBounds(t *testing.T) {
	d := newChildEngineServer(t)
	parent, worker, _ := childRelay(t, d)
	args := childGuestCreate(d, "one")
	args["total"] = 1
	id := childResponseID(t, childCall(t, parent, "child_create", args))
	d.srv.children.dispatch(context.Background())
	report := strings.Repeat("<", store.MaxStepReportBytes)
	if r := childCall(t, worker, "child_complete", map[string]any{"child_id": id, "request_id": "one", "outcome": "success", "report": report}); !r.OK {
		t.Fatalf("bounded report rejected: %+v", r)
	}
	r := childCall(t, parent, "child_get", map[string]any{"child_id": id})
	var data struct {
		Report string `json:"report"`
	}
	if !r.OK || json.Unmarshal(r.Data, &data) != nil || data.Report != report {
		t.Fatalf("bounded full report status failed: %.200s", r.Text)
	}
	for _, field := range []string{"actor_uid", "project_id", "url"} {
		bad := childGuestCreate(d, "other")
		bad[field] = "forged"
		if r := childCall(t, parent, "child_create", bad); r.OK {
			t.Fatalf("identity selector accepted %s", field)
		}
	}
	// Same actor name but changed generation is not the authenticated principal.
	d.exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, d.worker.UID)
	if r := childCall(t, worker, "child_get", map[string]any{"child_id": id}); r.OK {
		t.Fatal("old authenticated generation read task")
	}
}

func TestChildGuestUnsentVersusUncertainCancellation(t *testing.T) {
	for _, notSent := range []bool{true, false} {
		t.Run(map[bool]string{true: "not_sent", false: "unconfirmed"}[notSent], func(t *testing.T) {
			d := newChildEngineServer(t)
			parent, _, received := childRelay(t, d, relay.MessageResponse{NotSent: notSent, Verdict: "no receipt"})
			args := childGuestCreate(d, "one")
			id := childResponseID(t, childCall(t, parent, "child_create", args))
			d.srv.children.dispatch(context.Background())
			r := childCall(t, parent, "child_create", args)
			want := "unconfirmed"
			if notSent {
				want = "failed"
			}
			if r.OK || r.Outcome != want {
				t.Fatalf("retry concealed delivery: %+v", r)
			}
			cancel := map[string]any{"parent_sequence": 1, "parent_request_id": args["parent_request_id"], "request_id": "one"}
			r = childCall(t, parent, "child_cancel", cancel)
			if r.OK != notSent {
				t.Fatalf("cancellation lost effect: %+v", r)
			}
			d.srv.children.dispatch(context.Background())
			if received.Load() != 1 {
				t.Fatal("retry resent child")
			}
			status := childCall(t, parent, "child_get", map[string]any{"child_id": id})
			if !status.OK {
				t.Fatalf("status unavailable: %+v", status)
			}
			if d.committed().State == "completed" {
				t.Fatal("cancellation completed parent with missing member")
			}
		})
	}
}

func TestChildGuestEarlyAnyQuorumRetainsObligations(t *testing.T) {
	for _, mode := range []string{"any", "quorum"} {
		t.Run(mode, func(t *testing.T) {
			policy := "mode: " + mode
			if mode == "quorum" {
				policy += ", quorum: 1"
			}
			d := newChildEngineServerWithPolicy(t, policy)
			parent, worker, _ := childRelay(t, d)
			oneArgs := childGuestCreate(d, "one")
			twoArgs := childGuestCreate(d, "two")
			oneArgs["mode"] = mode
			twoArgs["mode"] = mode
			if mode == "quorum" {
				oneArgs["quorum"] = 1
				twoArgs["quorum"] = 1
			}
			one := childResponseID(t, childCall(t, parent, "child_create", oneArgs))
			childResponseID(t, childCall(t, parent, "child_create", twoArgs))
			d.srv.children.dispatch(context.Background())
			if r := childCall(t, worker, "child_complete", map[string]any{"child_id": one, "request_id": "one", "outcome": "success", "report": "First success"}); !r.OK {
				t.Fatal(r.Text)
			}
			d.srv.children.dispatch(context.Background())
			r := childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": oneArgs["parent_request_id"], "wait_seconds": 0})
			var data struct {
				State  string `json:"state"`
				Open   int    `json:"open"`
				Joined bool   `json:"joined"`
			}
			if !r.OK || json.Unmarshal(r.Data, &data) != nil || data.State != "ready_success" || data.Open != 1 || data.Joined || r.Outcome != "accepted" {
				t.Fatalf("dishonest early join: %+v %+v", r, data)
			}
			v, err := d.srv.store.GetSwarmStep(context.Background(), d.swarm, 1)
			if err != nil || v.State != "inflight" {
				t.Fatalf("early parent closure: %+v %v", v, err)
			}
		})
	}
}

func TestChildGuestParallelCreation(t *testing.T) {
	d := newChildEngineServer(t)
	parent, _, received := childRelay(t, d)
	replies := make(chan relay.SwarmResponse, 2)
	for _, key := range []string{"one", "two"} {
		args := childGuestCreate(d, key)
		go func() { replies <- childCall(t, parent, "child_create", args) }()
	}
	first := childResponseID(t, <-replies)
	second := childResponseID(t, <-replies)
	if first == second || received.Load() != 0 {
		t.Fatal("parallel creation collapsed identities or sent before dispatch")
	}
	for _, id := range []string{first, second} {
		w, err := d.srv.store.SwarmChildWorkByID(context.Background(), id)
		if err != nil || w.Task.State != "pending" || w.Task.Delivery != "queued" {
			t.Fatalf("durable parallel intent: %+v %v", w, err)
		}
	}
}

func TestAutomaticChildLifecycleRefusesBeforeProvider(t *testing.T) {
	ctx := context.Background()
	d := newOwnedChildEngineServer(t)
	parent, _, _ := childRelay(t, d)
	childResponseID(t, childCall(t, parent, "child_create", childGuestCreate(d, "one")))
	d.srv.children.dispatch(ctx)
	pool, err := d.srv.store.GetManagedPool(ctx, d.worker.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	client, err := d.srv.managed.client(pool)
	if err != nil {
		t.Fatal(err)
	}
	pve, ok := client.(*managedFakePVE)
	if !ok {
		t.Fatal("fixture provider changed")
	}
	pve.mu.Lock()
	before := [4]int{pve.creates, pve.starts, pve.stops, pve.deletes}
	pve.mu.Unlock()
	for _, name := range []string{"stop", "reset", "delete"} {
		actor, err := d.srv.store.GetManagedActor(ctx, d.worker.UID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.srv.store.BeginManagedActorOperation(ctx, actor.UID, actor.Revision, store.NewID(), name); err == nil || !strings.Contains(err.Error(), "child") {
			t.Fatalf("automatic lifecycle crossed child hold: %v", err)
		}
	}
	pve.mu.Lock()
	after := [4]int{pve.creates, pve.starts, pve.stops, pve.deletes}
	pve.mu.Unlock()
	if after != before {
		t.Fatalf("lifecycle changed provider: %v -> %v", before, after)
	}
}

func TestChildGuestServerRestartRetainsSending(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	parent, _, received := childRelay(t, d)
	id := childResponseID(t, childCall(t, parent, "child_create", childGuestCreate(d, "one")))
	w, err := d.srv.store.SwarmChildWorkByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.srv.store.StartSwarmChild(ctx, w.Parent, w.Task.RequestID, w.Task.Executor, w.Task.Revision, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "restart.db")
	d.exec(`VACUUM INTO ?`, snapshot)
	restarted, err := New(ctx, Config{DBPath: snapshot}, Options{Provisioner: newFakeProvisioner()})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	recovered, err := restarted.store.SwarmChildWorkByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Task.State != "running" || recovered.Task.Delivery != "unconfirmed" || received.Load() != 0 {
		t.Fatalf("restart forgot or resent possible effects: %+v", recovered)
	}
	a, err := restarted.store.GetManagedActor(ctx, d.worker.UID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.store.BeginManagedActorOperation(ctx, a.UID, a.Revision, "reset-after-restart", "reset"); !errors.Is(err, store.ErrCoordinationConflict) {
		t.Fatalf("restart released child hold: %v", err)
	}
}

func TestChildGuestBoundedJoinObservesCompletion(t *testing.T) {
	d := newChildEngineServer(t)
	parent, worker, _ := childRelay(t, d)
	args := childGuestCreate(d, "one")
	args["total"] = 1
	id := childResponseID(t, childCall(t, parent, "child_create", args))
	d.srv.children.dispatch(context.Background())
	joined := make(chan relay.SwarmResponse, 1)
	go func() {
		joined <- childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": args["parent_request_id"], "wait_seconds": 2})
	}()
	if r := childCall(t, worker, "child_complete", map[string]any{"child_id": id, "request_id": "one", "outcome": "fail", "report": "Independent failure report"}); !r.OK {
		t.Fatalf("completion: %+v", r)
	}
	select {
	case r := <-joined:
		if !r.OK || r.Outcome != "completed" {
			t.Fatalf("bounded join failed to observe completion: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bounded join did not return")
	}
}

func TestChildGuestAmbiguousMutationErrorsRemainUnconfirmed(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("private-sql-sentinel")} {
		r := childCommandRefusal(err)
		if r.OK || r.Outcome != "unconfirmed" || len(r.Data) != 0 || strings.Contains(r.Text, "private-sql-sentinel") {
			t.Fatalf("ambiguous mutation misreported: %+v", r)
		}
	}
}

func TestChildGuestJoinCancellationIsBounded(t *testing.T) {
	d := newChildEngineServer(t)
	// Direct authenticated adapter context tests cancellation without waiting 15s.
	raw, _ := json.Marshal(childGuestCreate(d, "one"))
	r := d.srv.managed.handleSwarmRequest(context.Background(), d.master.UID, d.master.Incarnation, relay.SwarmRequest{Tool: "child_create", Arguments: raw})
	if !r.OK {
		t.Fatalf("create: %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, _ = json.Marshal(map[string]any{"parent_sequence": 1, "parent_request_id": d.committed().Request, "wait_seconds": 15})
	start := time.Now()
	r = d.srv.managed.handleSwarmRequest(ctx, d.master.UID, d.master.Incarnation, relay.SwarmRequest{Tool: "child_join", Arguments: raw})
	if time.Since(start) > time.Second || r.Outcome == "completed" {
		t.Fatalf("cancelled join blocked or completed: %+v", r)
	}
}
