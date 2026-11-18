package server

// FEATURE: Одна durable-инструкция зарегистрированному ephemeral child (#292).
// SPEC: FULL-резерв до эффекта, неизменные bytes/identity, без legacy fallback;
// recovery читает только retained child intents, receipt не завершает child.
// IMPLEMENTATION: FULL-подготовка, общий sender и отдельный bounded child cursor.
// RESULT: IMPLEMENTED — исходные bytes сохраняются, legacy fallback отсутствует.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type ephemeralDeliveryController interface {
	deliverEphemeralChild(context.Context, string, store.SwarmChildParent, string) (store.ActorDeliveryIntent, string, error)
}
type ephemeralDeliveryRecovery interface {
	recoverChildren(context.Context, string) (string, error)
}

type childOfflineRelay struct{ guestRelay }

func (childOfflineRelay) Connected(string, int64) bool { return false }

func childDeliveryCall(t *testing.T, d *dispatchServer, user string, parent store.SwarmChildParent, id string) (store.ActorDeliveryIntent, string, error) {
	t.Helper()
	controller, ok := any(d.srv).(ephemeralDeliveryController)
	if !ok {
		t.Fatal("missing trusted ephemeral child delivery controller")
	}
	return controller.deliverEphemeralChild(context.Background(), user, parent, id)
}
func recoverChildDeliveries(t *testing.T, d *dispatchServer) {
	t.Helper()
	worker, ok := any(d.srv.actorDelivery).(ephemeralDeliveryRecovery)
	if !ok {
		t.Fatal("missing typed retained child recovery")
	}
	if _, err := worker.recoverChildren(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func childDeliveryFixture(t *testing.T) (*dispatchServer, store.EphemeralChildIntent, store.SwarmChildTask, *durableAnswerRelay, *atomic.Int64) {
	t.Helper()
	d, intent, child := ephemeralChildServerFixture(t)
	d.srv.actorDelivery.close()
	hub := &durableAnswerRelay{guestRelay: d.srv.managed.hub, uid: child.Executor.ActorUID, incarnation: child.Executor.Incarnation, journal: strings.Repeat("a", 64)}
	d.srv.managed.hub = hub
	clock := new(atomic.Int64)
	clock.Store(time.Now().UTC().UnixNano())
	d.srv.actorDelivery = newActorDeliveryService(d.srv, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	return d, intent, child, hub, clock
}

func TestEphemeralDeliveryCommitsBytesAndRunningBeforeEffect(t *testing.T) {
	d, job, child, hub, _ := childDeliveryFixture(t)
	var received relay.MessageRequest
	var calls atomic.Int32
	d.srv.managed.deliverMessage = func(_ context.Context, uid string, incarnation int64, req relay.MessageRequest) (relay.MessageResponse, error) {
		calls.Add(1)
		received = req
		raw, err := sql.Open("sqlite", d.dbPath)
		if err != nil {
			t.Error(err)
			return relay.MessageResponse{}, err
		}
		defer raw.Close()
		var state, payload, journal, source string
		err = raw.QueryRow(`SELECT c.state,d.payload,d.journal_id,d.source_id FROM swarm_child_tasks c JOIN actor_deliveries d ON d.source_id=c.id WHERE d.id=?`, req.DeliveryID).Scan(&state, &payload, &journal, &source)
		if err != nil || state != "running" || journal != hub.journal || source != child.ID {
			t.Errorf("effect before durable start: %q %q %q %v", state, journal, source, err)
		}
		var retained relay.MessageRequest
		if json.Unmarshal([]byte(payload), &retained) != nil || retained.Body != req.Body || retained.To != req.To || retained.From != req.From || retained.DeliveryID != "" {
			t.Error("retained original payload differs")
		}
		history, e := d.srv.store.GetActorDeliveryHistory(context.Background(), req.DeliveryID)
		if e != nil || len(history) != 1 || history[0].Outcome != "" {
			t.Errorf("effect before attempt: %+v %v", history, e)
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	before, _ := d.calls()
	retained, outcome, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID)
	if err != nil || outcome != "delivered" || calls.Load() != 1 {
		t.Fatalf("delivery: %+v %q %v calls=%d", retained, outcome, err, calls.Load())
	}
	actor, err := d.srv.store.GetManagedActor(context.Background(), child.Executor.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	if received.To != actor.Name || received.From != stepMessageSender(d.swarm) || !strings.Contains(received.Body, job.Payload) || !strings.Contains(received.Body, child.ID) || retained.Input.Commit != job.SourceCommit || retained.Input.RequestID != job.RequestID {
		t.Fatalf("wrong instruction scope: %+v %+v", received, retained)
	}
	retry, outcome, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID)
	if err != nil || outcome != "duplicate" || retry != retained || calls.Load() != 1 {
		t.Fatalf("exact retry changed delivery: %+v %q %v", retry, outcome, err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || work.Task.State != "running" || work.Task.Outcome != "" || work.Task.Report != "" {
		t.Fatalf("receipt completed child: %+v %v", work, err)
	}
	if err := d.srv.managed.completeChild(context.Background(), actor, child.ID, child.RequestID, "success", "Independent result retained outside worker."); err != nil {
		t.Fatal(err)
	}
	work, err = d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || work.Task.State != "completed" || work.Task.Report == "" {
		t.Fatalf("independent report not recorded: %+v %v", work, err)
	}
	after, _ := d.calls()
	if len(before) != len(after) {
		t.Fatal("delivery repeated attestation")
	}
}

func TestEphemeralDeliveryConcurrentExactCallsWriteOnce(t *testing.T) {
	d, job, _, _, _ := childDeliveryFixture(t)
	var calls atomic.Int32
	d.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		calls.Add(1)
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID)
			if err != nil {
				t.Errorf("exact call: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent writes=%d", calls.Load())
	}
}

func TestEphemeralDeliveryRefusesWithoutPartialStart(t *testing.T) {
	for _, scenario := range []string{"owner", "owner-after-preflight", "viewer", "parent", "role", "source", "namespace", "external-namespace", "legacy", "missing-journal", "invalid-journal", "offline"} {
		t.Run(scenario, func(t *testing.T) {
			d, job, child, hub, _ := childDeliveryFixture(t)
			user, parent := d.master.DelegatorID, job.Parent
			raw, err := sql.Open("sqlite", d.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			switch scenario {
			case "owner":
				err = d.srv.store.SetUserDisabled(context.Background(), user, true)
			case "owner-after-preflight":
				d.fake.mu.Lock()
				d.fake.beforeNamespaceGet = func() {
					// Отдельная Store-запись завершится только если namespace preflight
					// действительно находится вне delivery-транзакции.
					if err := d.srv.store.SetUserDisabled(context.Background(), user, true); err != nil {
						t.Error(err)
					}
				}
				d.fake.mu.Unlock()
			case "external-namespace":
				d.fake.mu.Lock()
				d.fake.namespaceError = errors.New("namespace unavailable")
				d.fake.mu.Unlock()
			case "viewer":
				var viewer store.User
				viewer, err = d.srv.store.GetUserByUsername(context.Background(), "bob")
				user = viewer.ID
			case "parent":
				parent.Pass++
			case "role":
				_, err = raw.Exec(`UPDATE managed_actors SET role='worker' WHERE uid=?`, d.master.UID)
			case "source":
				_, err = raw.Exec(`UPDATE managed_actors SET origin='https://foreign.invalid/repo' WHERE uid=?`, child.Executor.ActorUID)
			case "namespace":
				_, err = raw.Exec(`UPDATE swarms SET atespace_uid='replacement' WHERE id=?`, d.swarm)
			case "legacy":
				d.srv.managed.hub = hub.guestRelay
			case "missing-journal":
				hub.setJournal("")
			case "invalid-journal":
				hub.setJournal(strings.Repeat("A", 64))
			case "offline":
				hub.guestRelay = childOfflineRelay{hub.guestRelay}
			}
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			d.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
				calls.Add(1)
				return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
			}
			if _, _, err := childDeliveryCall(t, d, user, parent, job.ID); err == nil {
				t.Fatal("untrusted delivery accepted")
			}
			var state string
			var n int
			if err := raw.QueryRow(`SELECT state FROM swarm_child_tasks WHERE id=?`, child.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow(`SELECT count(*) FROM actor_deliveries WHERE source_kind='child'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if state != "pending" || n != 0 || calls.Load() != 0 {
				t.Fatalf("partial start/fallback: state=%s intents=%d calls=%d", state, n, calls.Load())
			}
		})
	}
}

func TestEphemeralDeliveryRetainsIdentityAndTypedRecovery(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "not-sent"}[known], func(t *testing.T) {
			d, job, child, _, clock := childDeliveryFixture(t)
			var messages []relay.MessageRequest
			d.srv.managed.deliverMessage = func(_ context.Context, _ string, _ int64, req relay.MessageRequest) (relay.MessageResponse, error) {
				messages = append(messages, req)
				if len(messages) == 1 {
					if known {
						return relay.MessageResponse{NotSent: true, Verdict: "no write"}, nil
					}
					return relay.MessageResponse{}, errors.New("lost receipt")
				}
				return relay.MessageResponse{OK: true, Duplicate: !known, Verdict: "confirmed"}, nil
			}
			retained, _, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID); !errors.Is(err, store.ErrActorDeliveryPending) {
				t.Fatalf("backoff bypassed: %v", err)
			}
			recoverChildDeliveries(t, d)
			if len(messages) != 1 {
				t.Fatal("worker bypassed backoff")
			}
			for _, list := range []func(context.Context, string, int) ([]store.ActorDeliveryCandidate, error){d.srv.store.PendingActorDeliveryAnswers, d.srv.store.PendingActorDeliverySteps} {
				rows, err := list(context.Background(), "", 16)
				if err != nil || len(rows) != 0 {
					t.Fatalf("child leaked into legacy reader: %+v %v", rows, err)
				}
			}
			before, _ := d.calls()
			clock.Add(int64(time.Hour))
			d.srv.actorDelivery = newActorDeliveryService(d.srv, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
			recoverChildDeliveries(t, d)
			if len(messages) != 2 {
				t.Fatalf("restart did not resume: %d", len(messages))
			}
			first, second := messages[0], messages[1]
			if first.DeliveryID != retained.ID || first.DeliveryID != second.DeliveryID || first.JournalID != second.JournalID || first.Body != second.Body || first.To != second.To || first.From != second.From || second.Replay == known {
				t.Fatalf("recovery changed identity/protocol: %+v %+v", first, second)
			}
			again, err := d.srv.store.GetActorDelivery(context.Background(), retained.ID)
			if err != nil || again != retained {
				t.Fatalf("reconstructed message: %+v %v", again, err)
			}
			after, _ := d.calls()
			if len(before) != len(after) {
				t.Fatal("recovery repeated attestation")
			}
			work, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
			if err != nil || work.Task.State != "running" {
				t.Fatalf("receipt completed child: %+v %v", work, err)
			}
		})
	}
}

func TestEphemeralDeliveryResumePreservesPriorRendererBytes(t *testing.T) {
	d, job, child, hub, clock := childDeliveryFixture(t)
	actor, err := d.srv.store.GetManagedActor(context.Background(), child.Executor.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	// Снимок предыдущей версии server renderer намеренно не совпадает с
	// сегодняшним текстом/JSON formatting. Resume не вправе его нормализовать.
	payload, err := json.MarshalIndent(relay.MessageRequest{To: actor.Name, From: stepMessageSender(d.swarm), Body: "Frozen original instruction.\n\n" + job.Payload + "\nReport using child_complete."}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	original, err := d.srv.store.PrepareEphemeralChildDelivery(context.Background(), job.ID, store.ActorDeliveryInput{SwarmID: d.swarm, SourceKind: "child", SourceID: child.ID, RequestID: job.RequestID, Commit: job.SourceCommit, ActorUID: actor.UID, Incarnation: actor.Incarnation, AtespaceUID: actor.AtespaceUID, JournalID: hub.journal, Payload: string(payload)}, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	var messages []relay.MessageRequest
	d.srv.managed.deliverMessage = func(_ context.Context, _ string, _ int64, req relay.MessageRequest) (relay.MessageResponse, error) {
		messages = append(messages, req)
		return relay.MessageResponse{}, errors.New("lost receipt")
	}
	got, _, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID)
	if err != nil || got != original {
		t.Fatalf("foreground rerendered retained instruction: %+v %v", got, err)
	}
	clock.Add(int64(time.Hour))
	d.srv.actorDelivery = newActorDeliveryService(d.srv, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	recoverChildDeliveries(t, d)
	retained, err := d.srv.store.GetActorDelivery(context.Background(), original.ID)
	if err != nil || retained != original || len(messages) != 2 || !messages[1].Replay || messages[0].Body != messages[1].Body || messages[0].Body != "Frozen original instruction.\n\n"+job.Payload+"\nReport using child_complete." {
		t.Fatalf("restart normalized/reconstructed bytes: %+v messages=%+v %v", retained, messages, err)
	}
}

func TestEphemeralDeliveryDoesNotRegisterOrAttestMissingProof(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-proof", true: "unknown-proof"}[unknown], func(t *testing.T) {
			d, job, allocation := ephemeralAttestationServerFixture(t)
			d.srv.actorDelivery.close()
			if unknown {
				d.reply = func(guestAttest) (relay.SessionResponse, error) {
					return relay.SessionResponse{Outcome: "unknown"}, nil
				}
				_, _ = d.srv.attestEphemeralChild(context.Background(), d.master.DelegatorID, job.Parent, job.ID, "unknown-proof")
			}
			d.srv.managed.hub = &durableAnswerRelay{guestRelay: d.srv.managed.hub, uid: allocation.ActorUID, incarnation: allocation.ActorIncarnation, journal: strings.Repeat("a", 64)}
			before, messages := d.calls()
			if _, _, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID); err == nil {
				t.Fatal("missing registered proof accepted")
			}
			recoverChildDeliveries(t, d)
			after, sent := d.calls()
			if len(before) != len(after) || len(messages) != len(sent) {
				t.Fatal("controller repeated attestation or sent legacy instruction")
			}
			raw, err := sql.Open("sqlite", d.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			var children, deliveries int
			if err := raw.QueryRow(`SELECT (SELECT count(*) FROM swarm_child_tasks),(SELECT count(*) FROM actor_deliveries WHERE source_kind='child')`).Scan(&children, &deliveries); err != nil || children != 0 || deliveries != 0 {
				t.Fatalf("controller reconstructed registration/delivery: %d %d %v", children, deliveries, err)
			}
		})
	}
}

func TestEphemeralDeliveryWorkerRecoversOnlyPreparedMessagesAndCloses(t *testing.T) {
	d, job, child, _, _ := childDeliveryFixture(t)
	before, messages := d.calls()
	recoverChildDeliveries(t, d)
	after, sent := d.calls()
	work, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || work.Task.State != "pending" || len(before) != len(after) || len(messages) != len(sent) {
		t.Fatalf("recovery activated unprepared child: %+v %v", work, err)
	}
	retained, err := d.srv.prepareEphemeralChildDelivery(context.Background(), d.master.DelegatorID, job.Parent, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	d.srv.managed.deliverMessage = func(ctx context.Context, _ string, _ int64, req relay.MessageRequest) (relay.MessageResponse, error) {
		if req.DeliveryID != retained.ID {
			t.Error("worker reconstructed delivery identity")
		}
		close(entered)
		<-ctx.Done()
		return relay.MessageResponse{}, ctx.Err()
	}
	d.srv.actorDelivery.start(context.Background())
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("service did not scan prepared child")
	}
	closed := make(chan struct{})
	go func() { d.srv.actorDelivery.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(7 * time.Second):
		t.Fatal("service did not cancel child transport on close")
	}
}

func TestEphemeralDeliveryCancelledForegroundDoesNotWaitForBusyIntent(t *testing.T) {
	d, job, _, _, _ := childDeliveryFixture(t)
	retained, err := d.srv.prepareEphemeralChildDelivery(context.Background(), d.master.DelegatorID, job.Parent, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	unlock := d.srv.deliveries.lock(retained.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := d.srv.deliverEphemeralChild(ctx, d.master.DelegatorID, job.Parent, job.ID)
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wrong cancellation: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		unlock()
		<-done
		t.Fatal("cancelled foreground waited for another delivery's lock")
	}
}

func TestEphemeralDeliveryCancellationDuringNamespacePreflight(t *testing.T) {
	d, job, child, _, _ := childDeliveryFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	d.fake.mu.Lock()
	d.fake.beforeNamespaceGet = func() {
		close(entered)
		<-release
	}
	d.fake.mu.Unlock()
	var sent atomic.Int32
	d.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		sent.Add(1)
		return relay.MessageResponse{}, errors.New("unexpected delivery")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := d.srv.deliverEphemeralChild(ctx, d.master.DelegatorID, job.Parent, job.ID)
		done <- err
	}()
	select {
	case <-entered:
		cancel()
	case err := <-done:
		t.Fatalf("preflight not reached: %v", err)
	case <-time.After(time.Second):
		t.Fatal("preflight not reached")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancellation lost at namespace preflight: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("namespace preflight did not stop on cancellation")
	}
	work, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || work.Task.State != child.State || work.Task.Revision != child.Revision || sent.Load() != 0 {
		t.Fatalf("canceled preflight changed state or sent: %+v error=%v sends=%d", work.Task, err, sent.Load())
	}
	if _, err := d.srv.store.GetActorDeliveryForSource(context.Background(), d.swarm, "child", child.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("canceled preflight created an intent: %v", err)
	}
}

func TestEphemeralDeliveryRecoveryRefusesChangedAuthorityAndBudget(t *testing.T) {
	for _, scenario := range []string{"journal", "owner", "stop", "rebind", "parent-role", "budget"} {
		t.Run(scenario, func(t *testing.T) {
			d, job, child, hub, clock := childDeliveryFixture(t)
			var calls atomic.Int32
			d.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
				calls.Add(1)
				return relay.MessageResponse{NotSent: true, Verdict: "no write"}, nil
			}
			retained, _, err := childDeliveryCall(t, d, d.master.DelegatorID, job.Parent, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", d.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			switch scenario {
			case "journal":
				hub.setJournal(strings.Repeat("b", 64))
			case "owner":
				err = d.srv.store.SetUserDisabled(context.Background(), d.master.DelegatorID, true)
			case "stop":
				_, err = raw.Exec(`UPDATE swarms SET state='stopped' WHERE id=?`, d.swarm)
			case "rebind":
				_, err = raw.Exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, child.Executor.ActorUID)
			case "parent-role":
				_, err = raw.Exec(`UPDATE managed_actors SET role='worker' WHERE uid=?`, d.master.UID)
			case "budget":
				for range 12 {
					clock.Add(int64(time.Hour))
					recoverChildDeliveries(t, d)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			clock.Add(int64(time.Hour))
			recoverChildDeliveries(t, d)
			if calls.Load() != before {
				t.Fatalf("recovery bypassed %s: %d -> %d", scenario, before, calls.Load())
			}
			if scenario == "budget" {
				schedule, err := d.srv.store.GetActorDeliverySchedule(context.Background(), retained.ID)
				if err != nil || !schedule.Exhausted {
					t.Fatalf("budget not exhausted: %+v %v", schedule, err)
				}
			}
		})
	}
}
