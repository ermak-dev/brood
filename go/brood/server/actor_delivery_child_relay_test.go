package server

// FEATURE: Финальный relay fence для parent и executor ephemeral-инструкции (#292).
// SPEC: Настоящие TLS/WSS/SSH и durable journal; задержанный adapter не обходит
// завершённое owner-вмешательство, потерянная квитанция разрешает только Lookup.
// IMPLEMENTATION: Общий sender несёт оба UID до acquire-many и локального tx check.
// RESULT: IMPLEMENTED — late writes отсутствуют; probe не повторяет запись в модель.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type childDeliveryReceiver struct {
	mu       sync.Mutex
	messages []relay.MessageRequest
	writes   atomic.Int32
	journal  *deliverylog.Journal
	executor relay.SwarmCaller
}

// Приёмник моделирует только запись в модель: transport, admission и квитанции
// проходят настоящий relay и файловый журнал, а не подменённый sender callback.
func childDeliveryRealRelay(t *testing.T, d *dispatchServer, child store.SwarmChildTask) *childDeliveryReceiver {
	t.Helper()
	return scopedDeliveryRealRelay(t, d, child, "child")
}

func scopedDeliveryRealRelay(t *testing.T, d *dispatchServer, child store.SwarmChildTask, sourceKind string) *childDeliveryReceiver {
	t.Helper()
	if sourceKind != "child" && sourceKind != "answer" {
		t.Fatal("unsupported local relay fixture source")
	}
	ctx, cancel := context.WithCancel(context.Background())
	actor, err := d.srv.store.GetManagedActor(ctx, child.Executor.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	d.worker = actor
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := deliverylog.Open(ctx, directory, deliverylog.Scope{ActorUID: actor.UID, Incarnation: actor.Incarnation}, deliverylog.MaxEntries)
	if err != nil {
		t.Fatal(err)
	}
	receiver := &childDeliveryReceiver{journal: journal}
	m := d.srv.managed
	hub, err := relay.NewHub([]byte(m.registry.relaySecret), m.authorizeGuest)
	if err != nil {
		t.Fatal(err)
	}
	m.hub = hub
	m.deliverMessage = m.deliverViaHub
	m.enableSwarmMessages()
	server := httptest.NewTLSServer(hub.Handler())
	done := make(chan error, 2)
	calls := make(chan struct {
		uid  string
		call relay.SwarmCaller
	}, 2)
	for _, a := range []store.ManagedActor{d.master, actor} {
		cfg := relay.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https"), ActorUID: a.UID, Generation: a.Incarnation, Token: guestToken(m.registry.relaySecret, a.UID, a.Incarnation), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), HostKey: hub.HostKey(), DialTarget: func(context.Context) (net.Conn, error) { return nil, errors.New("unused terminal") }, Session: func(context.Context, relay.SessionRequest) relay.SessionResponse {
			return relay.SessionResponse{OK: true, Outcome: "submitted", Text: "owner input submitted"}
		}, OnSwarmSession: func(ctx context.Context, call relay.SwarmCaller) {
			calls <- struct {
				uid  string
				call relay.SwarmCaller
			}{a.UID, call}
			<-ctx.Done()
		}}
		if a.UID == actor.UID {
			cfg.MessageDeduplication = true
			cfg.MessageJournalID = journal.Identifier()
			cfg.HandleMessage = func(ctx context.Context, req relay.MessageRequest) relay.MessageResponse {
				receiver.mu.Lock()
				receiver.messages = append(receiver.messages, req)
				receiver.mu.Unlock()
				if req.To != actor.Name || req.JournalID != journal.Identifier() {
					t.Error("foreign receiver identity")
					return relay.MessageResponse{Verdict: "wrong identity"}
				}
				canonical := req
				canonical.Replay = false
				payload, err := json.Marshal(canonical)
				if err != nil {
					t.Error(err)
					return relay.MessageResponse{}
				}
				if req.Replay {
					state, err := journal.Lookup(ctx, req.DeliveryID, payload)
					if err != nil {
						t.Error(err)
					}
					if state == deliverylog.Duplicate {
						return relay.MessageResponse{OK: true, Duplicate: true, Verdict: "retained receipt"}
					}
					return relay.MessageResponse{Verdict: "unconfirmed; read only"}
				}
				admission, err := journal.Begin(ctx, req.DeliveryID, payload)
				if err != nil {
					t.Error(err)
					return relay.MessageResponse{}
				}
				if admission.State == deliverylog.Duplicate {
					return relay.MessageResponse{OK: true, Duplicate: true, Verdict: "retained receipt"}
				}
				if admission.State != deliverylog.New {
					return relay.MessageResponse{Verdict: "uncertain; no write"}
				}
				raw, err := sql.Open("sqlite", d.dbPath)
				if err != nil {
					t.Error(err)
					return relay.MessageResponse{}
				}
				defer raw.Close()
				var state, retained string
				statement := `SELECT c.state,d.payload FROM swarm_child_tasks c JOIN actor_deliveries d ON d.source_id=c.id WHERE c.id=? AND d.id=? AND d.source_kind='child'`
				if sourceKind == "answer" {
					statement = `SELECT c.state,d.payload FROM swarm_child_tasks c JOIN actor_deliveries d ON d.actor_uid=c.actor_uid AND d.incarnation=c.actor_incarnation JOIN swarm_requests q ON q.id=d.source_id AND q.swarm_id=c.swarm_id AND q.actor_uid=c.actor_uid AND q.actor_incarnation=c.actor_incarnation WHERE c.id=? AND d.id=? AND d.source_kind='answer' AND q.origin='actor' AND q.state='answered'`
				}
				err = raw.QueryRow(statement, child.ID, req.DeliveryID).Scan(&state, &retained)
				var original relay.MessageRequest
				if err != nil || state != "running" || json.Unmarshal([]byte(retained), &original) != nil || original.Body != req.Body {
					t.Errorf("real effect before durable exact bytes/running: %s %v", state, err)
				}
				receiver.writes.Add(1)
				if err := journal.Finish(ctx, admission.Ticket, deliverylog.Delivered); err != nil {
					t.Error(err)
					return relay.MessageResponse{}
				}
				return relay.MessageResponse{OK: true, Verdict: "session write confirmed"}
			}
		}
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
				t.Error("client did not stop")
			}
		}
		journal.Close()
	})
	for range 2 {
		select {
		case c := <-calls:
			if c.uid == actor.UID {
				receiver.executor = c.call
			}
		case <-time.After(3 * time.Second):
			t.Fatal("guest did not connect")
		}
	}
	return receiver
}

// Используем существующий Store API, чтобы RED проверял настоящий общий sender,
// а не отсутствие нового controller-метода.
func childDeliveryPreparedFixture(t *testing.T) (*dispatchServer, store.EphemeralChildIntent, store.SwarmChildTask, store.ActorDeliveryIntent, *childDeliveryReceiver) {
	t.Helper()
	d, job, child := ephemeralChildServerFixture(t)
	d.srv.actorDelivery.close()
	receiver := childDeliveryRealRelay(t, d, child)
	allocation, err := d.srv.store.GetEphemeralChildAllocation(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := d.srv.store.GetEphemeralChildAttestation(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	text, err := ephemeralChildInstruction(ephemeralInstructionSource{Intent: job, Allocation: allocation, Proof: proof, Actor: d.worker, Work: store.SwarmChildWork{Parent: job.Parent, Task: child}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(relay.MessageRequest{To: d.worker.Name, From: stepMessageSender(d.swarm), Body: text})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := d.srv.store.PrepareEphemeralChildDelivery(context.Background(), job.ID, store.ActorDeliveryInput{SwarmID: d.swarm, SourceKind: "child", SourceID: child.ID, RequestID: job.RequestID, Commit: job.SourceCommit, ActorUID: d.worker.UID, Incarnation: d.worker.Incarnation, AtespaceUID: d.worker.AtespaceUID, JournalID: receiver.journal.Identifier(), Payload: string(payload)}, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	return d, job, child, retained, receiver
}

func TestEphemeralDeliveryActualSubmissionFencesBothActors(t *testing.T) {
	for _, target := range []string{"parent", "executor"} {
		t.Run(target, func(t *testing.T) {
			d, job, _, retained, receiver := childDeliveryPreparedFixture(t)
			entered, resume := make(chan struct{}), make(chan struct{})
			actual := d.srv.managed.deliverMessage
			d.srv.managed.deliverMessage = func(ctx context.Context, uid string, inc int64, req relay.MessageRequest) (relay.MessageResponse, error) {
				submission, ok := ctx.Value(messageSubmissionKey{}).(messageSubmission)
				parentIncluded, executorIncluded := false, false
				for _, uid := range submission.actors {
					parentIncluded = parentIncluded || uid == job.Parent.Principal.ActorUID
					executorIncluded = executorIncluded || uid == retained.Input.ActorUID
				}
				if !ok || !parentIncluded || !executorIncluded {
					t.Errorf("actual submission does not carry both immutable actor gates: %+v", submission.actors)
				}
				close(entered)
				select {
				case <-resume:
				case <-ctx.Done():
					return relay.MessageResponse{}, ctx.Err()
				}
				return actual(ctx, uid, inc, req)
			}
			local := func(ctx context.Context) bool {
				return d.srv.managed.childAuthority(ctx, store.SwarmChildAuthorization{Action: "start", Parent: job.Parent, Executor: childPrincipal(d.worker, job.Parent.Principal.ProjectID)}) == nil
			}
			done := make(chan struct{})
			go func() { defer close(done); _, _ = d.srv.actorDelivery.attempt(context.Background(), retained, local) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(resume)
				t.Fatal("adapter not entered")
			}
			name := d.worker.Name
			if target == "parent" {
				name = d.master.Name
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := d.srv.executeOperatorSessionCommand(ctx, d.master.DelegatorID, d.swarm, "operator:child-final-fence", coordinationInvocation("slash", map[string]any{"name": name, "command": "/status"}))
			cancel()
			close(resume)
			if err != nil {
				t.Fatalf("owner manual right denied: %v", err)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("delayed delivery did not finish")
			}
			if receiver.writes.Load() != 0 {
				t.Fatalf("%s intervention completed, then real guest received %d automatic writes", target, receiver.writes.Load())
			}
		})
	}
}

func TestEphemeralDeliveryRealJournalLostReceiptRecoversOnlyProbe(t *testing.T) {
	d, job, child, _, clock := childDeliveryFixture(t)
	receiver := childDeliveryRealRelay(t, d, child)
	actual := d.srv.managed.deliverMessage
	var sent atomic.Int32
	d.srv.managed.deliverMessage = func(ctx context.Context, uid string, inc int64, req relay.MessageRequest) (relay.MessageResponse, error) {
		response, err := actual(ctx, uid, inc, req)
		if err != nil || !response.OK {
			t.Logf("actual child transport: replay=%v response=%+v error=%v", req.Replay, response, err)
		}
		if sent.Add(1) == 1 && err == nil {
			return relay.MessageResponse{}, errors.New("receipt lost after actual journal commit")
		}
		return response, err
	}
	before, _ := d.calls()
	retained, outcome, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID)
	if err != nil || outcome != "unconfirmed" || receiver.writes.Load() != 1 {
		history, historyErr := d.srv.store.GetActorDeliveryHistory(context.Background(), retained.ID)
		t.Fatalf("lost receipt: %q %v writes=%d history=%+v historyError=%v", outcome, err, receiver.writes.Load(), history, historyErr)
	}
	clock.Add(int64(time.Hour))
	d.srv.actorDelivery = newActorDeliveryService(d.srv, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	recoverChildDeliveries(t, d)
	receiver.mu.Lock()
	messages := append([]relay.MessageRequest(nil), receiver.messages...)
	receiver.mu.Unlock()
	if len(messages) != 2 || !messages[1].Replay || messages[0].DeliveryID != retained.ID || messages[1].DeliveryID != retained.ID || messages[0].Body != messages[1].Body || receiver.writes.Load() != 1 {
		t.Fatalf("actual relay repeated write or changed identity: %+v writes=%d", messages, receiver.writes.Load())
	}
	work, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || work.Task.State != "running" || work.Task.Report != "" {
		t.Fatalf("receipt completed child: %+v %v", work, err)
	}
	response := childCall(t, receiver.executor, "child_complete", map[string]any{"child_id": child.ID, "request_id": child.RequestID, "outcome": "success", "report": "Independent result retained outside worker."})
	if !response.OK {
		t.Fatalf("independent report refused: %+v", response)
	}
	after, _ := d.calls()
	if len(before) != len(after) {
		t.Fatal("restart repeated root attestation")
	}
}
