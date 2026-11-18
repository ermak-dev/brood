package server

// FEATURE: update is a controlled restart onto the server's guest release (#133).
// SPEC: update restarts a running managed actor in its retained container,
// so the bootstrap installs the guest release the server serves now (#184);
// CTID, disk, owner nonce, generation, template and resources stay. The answer
// names the release before (as the guest root reports it) and after (the
// server's manifest) and says the template and resources do not change. It is
// refused for non-owner editors, before any guest call or provider effect,
// while the actor's swarm runs a pipeline pass (naming its active step) or has
// coordination holds. Explicit owner/delegated commands retain interruption
// evidence instead. It is refused for a stopped actor and without a verified server
// release. It is refused without a provider effect when the guest does not
// report its release (a runtime before #133 answers unsupported), for a plain
// bin of a bootstrap before #184 and for a container found stopped. An actor
// already on the server's release is not restarted. A master updates only workers it
// added; the operator managing the swarm updates any actor. An unconfirmed
// Proxmox stop is reported as an unknown outcome, never as acceptance, and the
// durable intent fences every concurrent lifecycle change while the stop is in
// flight; once the stop is confirmed and Start is not yet requested, update and
// start are refused naming the waiting start (#275: not the update, which the
// record does not tell apart from an ordinary start).
// IMPLEMENTATION: Server.updateActor over the shared lifecycle executor; the
// guest answer is a CallSession fake, Proxmox the managed fake.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// oldRelease — id набора, который гость фикстуры называет своим.
var oldRelease = strings.Repeat("1", 64)

func releaseReply(id string) relay.SessionResponse {
	data, _ := json.Marshal(relay.GuestRelease{Layout: "release", Release: id})
	return relay.SessionResponse{OK: true, Text: string(data), Outcome: "complete"}
}

// guestReleaseHub отвечает на guest_release ответом reply и записывает каждый
// вызов сессии как "<uid> <operation>"; прочие операции подтверждаются.
func guestReleaseHub(ts *substrateServer, reply func(uid string) (relay.SessionResponse, error)) func() []string {
	var mu sync.Mutex
	var calls []string
	next := ts.srv.managed.hub
	ts.srv.managed.hub = commandSessionHub{guestRelay: next, call: func(ctx context.Context, uid string, g int64, r relay.SessionRequest) (relay.SessionResponse, error) {
		mu.Lock()
		calls = append(calls, uid+" "+r.Operation)
		mu.Unlock()
		if r.Operation == relay.GuestReleaseOperation {
			return reply(uid)
		}
		if session, ok := next.(sessionRelay); ok {
			return session.CallSession(ctx, uid, g, r)
		}
		return relay.SessionResponse{OK: true, Text: "done", Outcome: "complete"}, nil
	}}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func reportsRelease(id string) func(string) (relay.SessionResponse, error) {
	return func(string) (relay.SessionResponse, error) { return releaseReply(id), nil }
}

// updateAnswer — ответ update любому вызывающему.
type updateAnswer struct {
	ok      bool
	outcome string
	text    string
	data    map[string]any
}

type updateCaller func(t *testing.T, name string) updateAnswer

func operatorUpdate(ts *substrateServer, client *http.Client, swarm string) updateCaller {
	return func(t *testing.T, name string) updateAnswer {
		t.Helper()
		r, b, raw := ts.do(client, "POST", "/api/commands/update", ts.csrf(client), map[string]any{"swarm_id": swarm, "arguments": map[string]any{"name": name}})
		got := updateAnswer{ok: r.StatusCode < 400}
		got.text, _ = b["text"].(string)
		if detail, refused := b["detail"].(string); refused {
			got.text = detail
		}
		if got.text == "" {
			t.Fatalf("update answer without text: %d %s", r.StatusCode, raw)
		}
		got.outcome, _ = b["outcome"].(string)
		got.data, _ = b["data"].(map[string]any)
		if r.Header.Get("X-Brood-Command-Outcome") == "unconfirmed" {
			got.outcome = "unknown"
		}
		switch {
		case got.outcome == "accepted" && r.StatusCode != http.StatusAccepted, got.outcome == "completed" && r.StatusCode != http.StatusOK:
			t.Fatalf("update status %d for %s: %s", r.StatusCode, got.outcome, raw)
		}
		return got
	}
}

func guestUpdate(ts *substrateServer, from store.ManagedActor) updateCaller {
	return func(t *testing.T, name string) updateAnswer {
		t.Helper()
		got := guestCommand(ts, from, "update", fmt.Sprintf(`{"name":%q}`, name))
		answer := updateAnswer{ok: got.OK, outcome: got.Outcome, text: got.Text}
		if len(got.Data) != 0 {
			if err := json.Unmarshal(got.Data, &answer.data); err != nil {
				t.Fatalf("update data: %v (%+v)", err, got)
			}
		}
		return answer
	}
}

func updateCallers(ts *substrateServer, alice *http.Client, master store.ManagedActor) map[string]updateCaller {
	return map[string]updateCaller{"operator": operatorUpdate(ts, alice, master.SwarmID), "master": guestUpdate(ts, master)}
}

// updateSetup — master, его worker и каталог артефактов сервера; target —
// id набора, который сервер раздаёт.
func updateSetup(t *testing.T) (ts *substrateServer, alice *http.Client, master, worker store.ManagedActor, pve *managedFakePVE, target string) {
	t.Helper()
	ts, alice, master, pve = masterSetup(t)
	worker = lifecycleWorker(t, ts, master, pve)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	target, ok := ts.srv.currentGuestRelease()
	if !ok || len(target) != 64 {
		t.Fatalf("server release: %q %v", target, ok)
	}
	return ts, alice, master, worker, pve, target
}

func TestUpdateRestartsOntoCurrentRelease(t *testing.T) {
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			ts, alice, master, worker, pve, target := updateSetup(t)
			fakeRelay := ts.srv.managed.hub.(*managedFakeRelay)
			calls := guestReleaseHub(ts, reportsRelease(oldRelease))
			before := providerEffectsOf(pve)
			got := updateCallers(ts, alice, master)[via](t, "worker")
			// Как stop, перезапуск сразу отключает relay прежней загрузки гостя.
			fakeRelay.mu.Lock()
			disconnects := fakeRelay.disconnects
			fakeRelay.mu.Unlock()
			if disconnects != 1 {
				t.Fatalf("relay disconnects after update: %d", disconnects)
			}
			if !got.ok || got.outcome != "accepted" || got.data["uid"] != worker.UID || got.data["release_before"] != oldRelease || got.data["release_after"] != target {
				t.Fatalf("update: %+v", got)
			}
			for _, says := range []string{"from guest release " + oldRelease + " to " + target, "the profile's template and resources do not change", "asynchronous, not a completion or readiness confirmation"} {
				if !strings.Contains(got.text, says) {
					t.Errorf("update answer lacks %q: %s", says, got.text)
				}
			}
			intent, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
			if err != nil || intent.Operation != "start" || intent.Stage != "stop" || intent.Desired != "running" || intent.UPID != "stop-task" || intent.OperationID == worker.OperationID {
				t.Fatalf("update intent: %+v %v", intent, err)
			}
			if got := calls(); strings.Join(got, ",") != worker.UID+" "+relay.GuestReleaseOperation {
				t.Fatalf("guest calls: %v", got)
			}
			want := before
			want.stops++
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("update provider effects: %+v want %+v", got, want)
			}
			restarted := settleActor(t, ts, worker.UID, actorRunning)
			if restarted.OperationID != intent.OperationID || restarted.Incarnation != worker.Incarnation || restarted.CTID != worker.CTID || restarted.OwnerNonce != worker.OwnerNonce || restarted.Resources != worker.Resources || restarted.Template != worker.Template || restarted.Origin != worker.Origin || restarted.ParentUID != master.UID {
				t.Fatalf("update replaced the allocation: %+v -> %+v", worker, restarted)
			}
			want.starts++
			want.bootstraps++
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("restart provider effects: %+v want %+v", got, want)
			}
		})
	}
}

// TestUpdateRefusesUnknownCurrentRelease: набор, о котором гость не
// сообщил, — отказ без намерения и эффекта провайдера. Runtime до #133
// отвечает unsupported, и перезапуск CT с bootstrap до #184 оставил бы её
// остановленной навсегда: отказ объясняет разовый stop и start и границу
// шаблона #184.
func TestUpdateRefusesUnknownCurrentRelease(t *testing.T) {
	for name, tc := range map[string]struct {
		reply        func(string) (relay.SessionResponse, error)
		disconnected bool
		cause        string
	}{
		// Runtime до #133 не знает операции и отвечает unsupported.
		"old runtime": {reply: func(string) (relay.SessionResponse, error) {
			return relay.SessionResponse{Text: "unsupported session operation", Outcome: "unsupported"}, nil
		}, cause: "its guest runtime predates guest_release"},
		"relay error": {reply: func(string) (relay.SessionResponse, error) {
			return relay.SessionResponse{}, errors.New("guest relay unavailable")
		}, cause: "the guest relay call failed"},
		"relay disconnected": {reply: reportsRelease(oldRelease), disconnected: true, cause: "its guest relay is not connected"},
		"unreadable": {reply: func(string) (relay.SessionResponse, error) {
			return relay.SessionResponse{Text: "guest release is not readable", Outcome: "failed"}, nil
		}, cause: "the guest did not confirm it"},
		// OK без исхода complete — не подтверждённый ответ, даже с
		// правильным телом.
		"ok without complete": {reply: func(string) (relay.SessionResponse, error) {
			reply := releaseReply(oldRelease)
			reply.Outcome = "failed"
			return reply, nil
		}, cause: "the guest did not confirm it"},
		"malformed answer": {reply: func(string) (relay.SessionResponse, error) {
			return relay.SessionResponse{OK: true, Text: `{"layout":"release","release":"latest"}`, Outcome: "complete"}, nil
		}, cause: "the guest answer is malformed"},
	} {
		t.Run(name, func(t *testing.T) {
			ts, alice, master, worker, pve, _ := updateSetup(t)
			fakeRelay := ts.srv.managed.hub.(*managedFakeRelay)
			guestReleaseHub(ts, tc.reply)
			if tc.disconnected {
				fakeRelay.mu.Lock()
				fakeRelay.connected = false
				fakeRelay.mu.Unlock()
			}
			effects := providerEffectsOf(pve)
			for via, call := range updateCallers(ts, alice, master) {
				t.Run(via, func(t *testing.T) {
					got := call(t, "worker")
					expectUpdateRefused(t, got, `actor "worker" did not report its current guest release (`+tc.cause+`)`, ts, pve, worker, effects)
					for _, says := range []string{"update does not restart a container whose release it cannot check", "a guest runtime older than #133 cannot report it", "a one-time stop and start installs one that can", "only in a container created from a template with the bootstrap of #184", "must not be stopped or restarted after a deploy that changed the guest artifacts (docs/PROXMOX.md)"} {
						if !strings.Contains(got.text, says) {
							t.Errorf("refusal lacks %q: %s", says, got.text)
						}
					}
				})
			}
		})
	}
}

// TestUpdateRefusesStoppedContainer: база считает актора работающим, а его
// CT остановлена мимо brood: update не останавливает и не запускает её,
// намерение не пишется.
func TestUpdateRefusesStoppedContainer(t *testing.T) {
	ts, alice, master, worker, pve, _ := updateSetup(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	pve.mu.Lock()
	pve.running = false
	pve.mu.Unlock()
	effects := providerEffectsOf(pve)
	for via, call := range updateCallers(ts, alice, master) {
		t.Run(via, func(t *testing.T) {
			expectUpdateRefused(t, call(t, "worker"), "managed lifecycle refused", ts, pve, worker, effects)
		})
	}
}

func TestUpdateLeavesCurrentReleaseRunning(t *testing.T) {
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			ts, alice, master, worker, pve, target := updateSetup(t)
			guestReleaseHub(ts, reportsRelease(target))
			effects := providerEffectsOf(pve)
			got := updateCallers(ts, alice, master)[via](t, "worker")
			if !got.ok || got.outcome != "completed" || got.data["release_before"] != target || got.data["release_after"] != target {
				t.Fatalf("update on the current release: %+v", got)
			}
			for _, says := range []string{"already runs guest release " + target, "nothing was restarted", "the profile's template and resources do not change"} {
				if !strings.Contains(got.text, says) {
					t.Errorf("no-op answer lacks %q: %s", says, got.text)
				}
			}
			expectUnchanged(t, ts, pve, worker, effects)
		})
	}
}

// expectUpdateRefused проверяет отказ без вызова гостя и эффекта провайдера.
func expectUpdateRefused(t *testing.T, got updateAnswer, says string, ts *substrateServer, pve *managedFakePVE, before store.ManagedActor, effects providerEffects) {
	t.Helper()
	if got.ok || got.outcome == "accepted" || got.outcome == "completed" || !strings.Contains(got.text, says) {
		t.Fatalf("update: %+v\nwant a refusal saying %s", got, says)
	}
	expectUnchanged(t, ts, pve, before, effects)
}

func TestUpdateRefusesStoppedActor(t *testing.T) {
	ts, alice, master, pve := masterSetup(t)
	worker := stoppedLifecycleWorker(t, ts, master, pve)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	calls := guestReleaseHub(ts, reportsRelease(oldRelease))
	effects := providerEffectsOf(pve)
	for via, call := range updateCallers(ts, alice, master) {
		t.Run(via, func(t *testing.T) {
			expectUpdateRefused(t, call(t, "worker"), `actor "worker" is stopped; update restarts a running actor; start it with start, which installs the server's current guest release`, ts, pve, worker, effects)
		})
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("refused update called the guest: %v", got)
	}
}

func TestUpdateRefusesWithoutServerRelease(t *testing.T) {
	ts, alice, master, worker, pve, _ := updateSetup(t)
	ts.srv.artifactsDir = ""
	calls := guestReleaseHub(ts, reportsRelease(oldRelease))
	effects := providerEffectsOf(pve)
	expectUpdateRefused(t, operatorUpdate(ts, alice, master.SwarmID)(t, "worker"), "the server has no verified guest release (BROOD_ARTIFACTS_DIR)", ts, pve, worker, effects)
	if got := calls(); len(got) != 0 {
		t.Fatalf("refused update called the guest: %v", got)
	}
}

func TestUpdateRefusesPlainBin(t *testing.T) {
	ts, alice, master, worker, pve, target := updateSetup(t)
	guestReleaseHub(ts, func(string) (relay.SessionResponse, error) {
		return relay.SessionResponse{OK: true, Text: `{"layout":"plain"}`, Outcome: "complete"}, nil
	})
	effects := providerEffectsOf(pve)
	for via, call := range updateCallers(ts, alice, master) {
		t.Run(via, func(t *testing.T) {
			expectUpdateRefused(t, call(t, "worker"), `actor "worker" runs a guest bootstrap before guest releases (#184): its /opt/brood/bin is a plain directory, so a restart cannot install release `+target, ts, pve, worker, effects)
		})
	}
}

func TestUpdateRefusesForeignActors(t *testing.T) {
	ts, _, master, other, theirs, pve := threeSlotMasters(t)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	calls := guestReleaseHub(ts, reportsRelease(oldRelease))
	effects := providerEffectsOf(pve)
	for _, target := range foreignTargets {
		got := guestUpdate(ts, master)(t, target.name)
		if got.ok || !strings.Contains(got.text, "not authorized") || !strings.Contains(got.text, target.reason) {
			t.Errorf("update of %s: %+v, want a refusal saying %s", target.name, got, target.reason)
		}
	}
	for _, a := range []store.ManagedActor{master, other, theirs} {
		expectUnchanged(t, ts, pve, a, effects)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("refused update called the guest: %v", got)
	}
	// Владелец worker и оператор обновляют.
	if got := guestUpdate(ts, other)(t, "theirs"); !got.ok || got.outcome != "accepted" {
		t.Fatalf("owning master update: %+v", got)
	}
}

func TestUpdateRefusesOperatorCreatedWorkerToMaster(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	calls := guestReleaseHub(ts, reportsRelease(oldRelease))
	got := guestUpdate(ts, master)(t, "worker")
	if got.ok || !strings.Contains(got.text, `"worker" was created by the swarm operator, not added by master "master"`) {
		t.Fatalf("master update of the operator's worker: %+v", got)
	}
	if after, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID); err != nil || after.Revision != worker.Revision {
		t.Fatalf("refusal changed the worker: %+v %v", after, err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("refused update called the guest: %v", got)
	}
}

func TestOperatorUpdatesMaster(t *testing.T) {
	ts, alice, master, pve := masterSetup(t)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	if got := operatorUpdate(ts, alice, master.SwarmID)(t, "master"); !got.ok || got.outcome != "accepted" || got.data["uid"] != master.UID {
		t.Fatalf("operator update of a master: %+v", got)
	}
	if restarted := settleActor(t, ts, master.UID, actorRunning); restarted.Incarnation != master.Incarnation || restarted.CTID != master.CTID {
		t.Fatalf("operator update replaced the master: %+v", restarted)
	}
	if effects := providerEffectsOf(pve); effects.stops != 1 || effects.creates != 1 || effects.deletes != 0 {
		t.Fatalf("operator update effects: %+v", effects)
	}
}

func TestEditorUpdateRefusesLandingToken(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	calls := guestReleaseHub(ts, reportsRelease(oldRelease))
	if _, err := ts.srv.managed.executeCoordinationCommand(context.Background(), master, coordinationInvocation("landing", map[string]any{"action": "take", "request_id": "land-133"})); err != nil {
		t.Fatal(err)
	}
	master, err := ts.srv.store.GetManagedActor(context.Background(), master.UID)
	if err != nil {
		t.Fatal(err)
	}
	effects := providerEffectsOf(pve)
	expectUpdateRefused(t, editorUpdate(t, ts, master.SwarmID)(t, "master"), `landing token "land-133" is held by "master"; the holder or an administrator gives it back with landing --action give --request-id "land-133"`, ts, pve, master, effects)
	if got := calls(); len(got) != 0 {
		t.Fatalf("refused update called the guest: %v", got)
	}
}

func TestEditorUpdateRefusesStepInProgress(t *testing.T) {
	d := newDispatchServer(t)
	// Worker добавлен master: отказ даёт шаг, а не владение.
	d.exec(`UPDATE managed_actors SET parent_uid=? WHERE uid=?`, d.master.UID, d.worker.UID)
	d.srv.artifactsDir = writeGuestArtifacts(t)
	calls := guestReleaseHub(d.substrateServer, reportsRelease(oldRelease))
	d.start("deliver")
	worker, err := d.srv.store.GetManagedActor(context.Background(), d.worker.UID)
	if err != nil {
		t.Fatal(err)
	}
	if step := d.committed(); step.State != "inflight" || step.Actor != worker.UID {
		t.Fatalf("step fixture: %+v", step)
	}
	before := calls()
	want := `pipeline step 1 (node "build") is inflight for actor "worker"; update restarts the actor and would interrupt it; wait for the step to finish or stop the swarm`
	for via, call := range map[string]updateCaller{"editor": editorUpdate(t, d.substrateServer, d.swarm)} {
		got := call(t, "worker")
		if got.ok || !strings.Contains(got.text, want) {
			t.Errorf("%s update with a step in progress: %+v\nwant a refusal saying %s", via, got, want)
		}
	}
	after, err := d.srv.store.GetManagedActor(context.Background(), worker.UID)
	if err != nil || after.Revision != worker.Revision || after.OperationID != worker.OperationID {
		t.Fatalf("refusal changed the worker: %+v %v", after, err)
	}
	if got := calls(); len(got) != len(before) {
		t.Fatalf("refused update called the guest: %v", got[len(before):])
	}
}

// Editor не получает owner-полномочий на прерывание прохода: human → build
// держит automatic update, хотя шаг build ещё не начат. Остановленный рой
// не держит актор. Явные owner/delegated команды проверяются отдельно.
func TestEditorUpdateRefusesWaitingPass(t *testing.T) {
	d := newDispatchServer(t)
	d.exec(`UPDATE managed_actors SET parent_uid=? WHERE uid=?`, d.master.UID, d.worker.UID)
	d.srv.artifactsDir = writeGuestArtifacts(t)
	calls := guestReleaseHub(d.substrateServer, reportsRelease(oldRelease))
	d.start("gated")
	if execution := d.execution(); execution != "waiting" {
		t.Fatalf("gated pass is %s, want waiting on the operator", execution)
	}
	worker, err := d.srv.store.GetManagedActor(context.Background(), d.worker.UID)
	if err != nil {
		t.Fatal(err)
	}
	want := `the pipeline pass of swarm "alpha" is waiting; update restarts actor "worker", and a step that becomes pending for its role during the restart would be refused; stop the swarm or wait for the pass to end`
	for via, call := range map[string]updateCaller{"editor": editorUpdate(t, d.substrateServer, d.swarm)} {
		if got := call(t, "worker"); got.ok || !strings.Contains(got.text, want) {
			t.Errorf("%s update during a waiting pass: %+v\nwant a refusal saying %s", via, got, want)
		}
	}
	after, err := d.srv.store.GetManagedActor(context.Background(), worker.UID)
	if err != nil || after.Revision != worker.Revision || after.OperationID != worker.OperationID {
		t.Fatalf("refusal changed the worker: %+v %v", after, err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("refused update called the guest: %v", got)
	}
	d.stop()
	if got := operatorUpdate(d.substrateServer, d.alice, d.swarm)(t, "worker"); !got.ok || got.outcome != "accepted" {
		t.Fatalf("update after the swarm stopped: %+v", got)
	}
}

func TestUpdateUncertainStopIsUnknown(t *testing.T) {
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			ts, alice, master, worker, pve, target := updateSetup(t)
			guestReleaseHub(ts, reportsRelease(oldRelease))
			pve.uncertainStop = true
			call := updateCallers(ts, alice, master)[via]
			got := call(t, "worker")
			if got.ok || got.outcome != "unknown" || strings.Contains(got.text, "accepted") || !strings.Contains(got.text, "outcome is unknown") || !strings.Contains(got.text, oldRelease) || !strings.Contains(got.text, target) {
				t.Fatalf("uncertain update: %+v", got)
			}
			intent, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
			if err != nil || intent.Operation != "start" || intent.Stage != "stop" || intent.UPID != "" || intent.Phase != "error" || intent.OperationID == worker.OperationID || !strings.Contains(got.text, intent.OperationID) {
				t.Fatalf("uncertain intent: %+v %v", intent, err)
			}
			// Неизвестный исход не повторяется: ни update, ни stop без сверки.
			effects := providerEffectsOf(pve)
			if again := call(t, "worker"); again.ok {
				t.Fatalf("update over an unknown outcome: %+v", again)
			}
			if ok, text, _ := lifecycleCallers(ts, alice, master)[via](t, "stop", `{"name":"worker"}`); ok {
				t.Fatalf("stop over an unknown update: %s", text)
			}
			expectUnchanged(t, ts, pve, intent, effects)
		})
	}
}

// TestUpdateUnrecordedStopIsUnknown: PVE принял остановку, а её UPID не
// записан (запись намерения сорвалась). Это неизвестный исход, а не отказ:
// оператор получает заголовок исхода, гость — unknown; намерение остаётся и
// держит актора.
func TestUpdateUnrecordedStopIsUnknown(t *testing.T) {
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			ts, alice, master, worker, pve, target := updateSetup(t)
			guestReleaseHub(ts, reportsRelease(oldRelease))
			pve.mu.Lock()
			pve.afterStop = func() { execDB(t, ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, worker.UID) }
			pve.mu.Unlock()
			call := updateCallers(ts, alice, master)[via]
			got := call(t, "worker")
			intent, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
			if err != nil || intent.Operation != "start" || intent.Stage != "stop" || intent.UPID != "" || intent.OperationID == worker.OperationID {
				t.Fatalf("unrecorded stop intent: %+v %v", intent, err)
			}
			if got.ok || got.outcome != "unknown" || !strings.Contains(got.text, `actor "worker" update outcome is unknown`) || !strings.Contains(got.text, oldRelease) || !strings.Contains(got.text, target) || !strings.Contains(got.text, intent.OperationID) {
				t.Fatalf("unrecorded stop: %+v", got)
			}
			pve.mu.Lock()
			pve.afterStop = nil
			pve.mu.Unlock()
			effects := providerEffectsOf(pve)
			if again := call(t, "worker"); again.ok {
				t.Fatalf("update over an unknown outcome: %+v", again)
			}
			expectUnchanged(t, ts, pve, intent, effects)
		})
	}
}

func TestUpdateFencesConcurrentLifecycle(t *testing.T) {
	ts, alice, master, worker, pve, _ := updateSetup(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	call := operatorUpdate(ts, alice, master.SwarmID)
	if got := call(t, "worker"); !got.ok || got.outcome != "accepted" {
		t.Fatalf("update: %+v", got)
	}
	// Пока остановка идёт, намерение держит актора от любого изменения; отказ
	// update не советует start. После подтверждённой остановки, пока Start не
	// запрошен, update и start получают отказ, называющий ожидание; stop,
	// delete и reset тогда отменяют update (command_update_cancel_test.go).
	for _, moment := range []string{"stopping", "start pending"} {
		if moment == "start pending" {
			if a := passActor(t, ts, worker.UID); a.Stage != "start" || a.UPID != "" {
				t.Fatalf("stop not confirmed: %+v", a)
			}
		}
		intent, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
		if err != nil {
			t.Fatal(err)
		}
		says := `actor "worker" is ` + intent.Phase + ` while its last lifecycle change is unfinished`
		changes := [][2]string{{"stop", `{"name":"worker"}`}, {"start", `{"name":"worker"}`}, {"reset", `{"name":"worker","confirm":true}`}}
		if moment == "start pending" {
			says = `actor "worker" is waiting to start its stopped container (Start is not yet requested)`
			changes = changes[1:2]
		}
		effects := providerEffectsOf(pve)
		if got := call(t, "worker"); got.ok || !strings.Contains(got.text, says) || strings.Contains(got.text, "start it with start") {
			t.Fatalf("second update while %s: %+v", moment, got)
		}
		lifecycle := operatorLifecycle(ts, alice, master.SwarmID)
		for _, change := range changes {
			if ok, text, _ := lifecycle(t, change[0], change[1]); ok || (moment == "start pending" && !strings.Contains(refusalText(text), says)) {
				t.Fatalf("%s while %s: %s", change[0], moment, text)
			}
		}
		expectUnchanged(t, ts, pve, intent, effects)
	}
}
