package server

// BUG: Отказ до записи PVE навсегда делает исход managed-актора неизвестным
// (#275). Start работающей CT с фазой error или provisioning пишет намерение,
// ConfigureBootstrap отказывает «must be stopped», и актор остаётся в стадии
// start без UPID; разовый отказ чтения владения в Stop оставляет стадию stop
// без UPID при работающей CT; неоднозначная запись bootstrap обычного start
// не повторяется; stop и reset остановленной CT шлют vzstop, на который PVE
// отвечает HTTP 500 «CT not running», а клиент считает его неоднозначным.
// Дальше stop, delete и reset отказывают.
// HYPOTHESIS: Любая ошибка launch записывается как неизвестный исход, хотя
// proxmox.Client отличает отказ до записи (без ErrUncertain и без UPID) от
// неоднозначной записи; start и stop не сверяются с наблюдаемой CT.
// SOLUTION: Start работающей CT согласуется наблюдением без записи PVE, а
// running публикует только проверка готовности. Start и stop, ещё не
// запрошенные у PVE, держат фазу наблюдённой CT (stopped у start, running у
// stop), и reconcile повторяет их каждым проходом, как start update (#133);
// перед отправкой фаза становится provisioning или stopping, и потерянный
// ответ остаётся неизвестным. Stop-стадия остановленной CT согласуется без
// vzstop.
// RESULT: FIXED
// STATUS: FIXED

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// unsentWorker — актор панели "worker", чья CT работает и гость готов.
type unsentWorker struct {
	ts    *substrateServer
	alice *http.Client
	call  lifecycleCaller
	pve   *managedFakePVE
	hub   *managedFakeRelay
	actor store.ManagedActor
}

func runningUnsentWorker(t *testing.T) unsentWorker {
	t.Helper()
	ts, alice, _, swarm, pve, hub := managedSetup(t)
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "worker"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	uid, _ := b["metadata"].(map[string]any)["uid"].(string)
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	a := passUntil(t, ts, uid, 4, actorRunning)
	return unsentWorker{ts: ts, alice: alice, call: panelLifecycle(ts, alice, swarm), pve: pve, hub: hub, actor: a}
}

// actorState — фаза и status.state актора worker в API.
func actorState(t *testing.T, ts *substrateServer, alice *http.Client, swarm string) (phase, state any) {
	t.Helper()
	r, b, _ := ts.do(alice, "GET", "/api/swarms/"+swarm+"/actors/worker", "", nil)
	expectStatus(t, "read worker", r, b, http.StatusOK)
	status, _ := b["status"].(map[string]any)
	return b["phase"], status["state"]
}

func (w unsentWorker) get(t *testing.T) store.ManagedActor {
	t.Helper()
	a, err := w.ts.srv.store.GetManagedActor(context.Background(), w.actor.UID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (w unsentWorker) fake(change func(*managedFakePVE)) {
	w.pve.mu.Lock()
	change(w.pve)
	w.pve.mu.Unlock()
}

func (w unsentWorker) guestReady(ready bool) {
	w.hub.mu.Lock()
	w.hub.dialError = nil
	if !ready {
		w.hub.dialError = errors.New("ttyd is not accepting connections")
	}
	w.hub.mu.Unlock()
}

// stopped останавливает актора и ждёт подтверждения.
func (w unsentWorker) stopped(t *testing.T) store.ManagedActor {
	t.Helper()
	if ok, text, _ := w.call(t, "stop", ""); !ok {
		t.Fatalf("stop: %s", text)
	}
	return passUntil(t, w.ts, w.actor.UID, 4, actorStopped)
}

func actorDeleted(a store.ManagedActor) bool { return a.Phase == "deleted" }

// stopWaiting — стадия stop, чей Stop PVE ещё не получил: CT работает,
// задачи нет, исход не неизвестный.
func stopWaiting(a store.ManagedActor) bool {
	return a.Stage == "stop" && a.UPID == "" && a.Phase == "running" && a.Error != "" && !strings.Contains(a.Error, uncertainOutcome)
}

// startWaiting — стадия start, чей Start PVE ещё не получил: CT остановлена.
func startWaiting(a store.ManagedActor) bool {
	return startPending(a) && a.Phase == "stopped" && a.Error != "" && !strings.Contains(a.Error, uncertainOutcome)
}

// TestManagedStartOfRunningContainerSettlesReadiness: start актора, чья CT
// работает, а фаза error или provisioning, ничего не пишет в PVE: запись
// bootstrap env работающей CT не положена, а второй Start не нужен.
// Намерение сразу согласовано наблюдением, running публикует только проверка
// готовности, и окно готовности считается заново. Stop и delete такого актора
// работают.
func TestManagedStartOfRunningContainerSettlesReadiness(t *testing.T) {
	for name, into := range map[string]func(*testing.T, unsentWorker){
		"readiness timed out": func(t *testing.T, w unsentWorker) {
			w.guestReady(false)
			w.ts.srv.managed.readiness.Store(w.actor.UID, managedReadiness{w.actor.OperationID, time.Now().Add(-3 * time.Minute)})
			if a := passActor(t, w.ts, w.actor.UID); a.Stage != "done" || a.Phase != "error" || a.Error != managedReadinessError {
				t.Fatalf("readiness timeout: %+v", a)
			}
		},
		"readiness pending": func(t *testing.T, w unsentWorker) {
			w.guestReady(false)
			if a := passActor(t, w.ts, w.actor.UID); a.Stage != "done" || a.Phase != "provisioning" {
				t.Fatalf("readiness wait: %+v", a)
			}
		},
		// Задача stop упала, CT продолжает работать.
		"stop task failed": func(t *testing.T, w unsentWorker) {
			w.guestReady(false)
			w.fake(func(f *managedFakePVE) {
				f.taskErrors = map[string]error{"stop-task": proxmox.ErrTaskFailed}
				f.afterStop = func() {
					w.pve.mu.Lock()
					w.pve.running = true
					w.pve.mu.Unlock()
				}
			})
			if ok, text, _ := w.call(t, "stop", ""); !ok {
				t.Fatalf("stop: %s", text)
			}
			a := passActor(t, w.ts, w.actor.UID)
			if a.Stage != "done" || a.Phase != "error" || a.Desired != "stopped" || a.Operation != "stop" {
				t.Fatalf("failed stop task: %+v", a)
			}
			w.fake(func(f *managedFakePVE) { f.taskErrors, f.afterStop = nil, nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := runningUnsentWorker(t)
			into(t, w)
			before := w.get(t)
			effects := providerEffectsOf(w.pve)
			if ok, text, _ := w.call(t, "start", ""); !ok {
				t.Fatalf("start of a running container: %s", text)
			}
			started := w.get(t)
			if started.OperationID == before.OperationID || started.Operation != "start" || started.Desired != "running" || started.Stage != "done" || started.Phase != "provisioning" || started.Error != "" || started.Incarnation != before.Incarnation || started.CTID != before.CTID {
				t.Fatalf("start of a running container: %+v", started)
			}
			if got := providerEffectsOf(w.pve); got != effects {
				t.Fatalf("start of a running container reached the provider: %+v want %+v", got, effects)
			}
			// Новое намерение — новое окно готовности: гость ещё не готов,
			// и это ожидание, а не прежний отказ.
			if a := passActor(t, w.ts, w.actor.UID); a.Stage != "done" || a.Phase != "provisioning" || a.Error != "" {
				t.Fatalf("readiness of the adopted start: %+v", a)
			}
			w.guestReady(true)
			if a := passActor(t, w.ts, w.actor.UID); !actorRunning(a) || a.OperationID != started.OperationID {
				t.Fatalf("ready guest of the adopted start: %+v", a)
			}
			if got := providerEffectsOf(w.pve); got != effects {
				t.Fatalf("readiness reached the provider: %+v want %+v", got, effects)
			}
			w.stopped(t)
			if ok, text, _ := w.call(t, "delete", ""); !ok {
				t.Fatalf("delete: %s", text)
			}
			passUntil(t, w.ts, w.actor.UID, 4, actorDeleted)
			want := effects
			want.stops++
			want.deletes++
			if got := providerEffectsOf(w.pve); got != want {
				t.Fatalf("stop and delete effects: %+v want %+v", got, want)
			}
		})
	}
}

// TestManagedStopRetriesFailureBeforeWrite: Stop, отказавший на чтении
// владения, PVE не получил: стадия stop ждёт со своей причиной, а не
// неизвестным исходом, и следующий проход отправляет её один раз. Так у
// всех операций, чья первая стадия — stop. Пока Stop ждёт, фаза API —
// наблюдённая running, а status.state — остановка, а не running.
func TestManagedStopRetriesFailureBeforeWrite(t *testing.T) {
	for action, tc := range map[string]struct {
		state  string
		done   func(store.ManagedActor) bool
		effect func(*providerEffects)
	}{
		"stop":   {state: "ACTOR_STATE_SUSPENDING", done: actorStopped, effect: func(e *providerEffects) { e.stops++ }},
		"delete": {state: "ACTOR_STATE_DELETING", done: actorDeleted, effect: func(e *providerEffects) { e.stops++; e.deletes++ }},
		"reset": {state: "ACTOR_STATE_SUSPENDING", done: func(a store.ManagedActor) bool { return actorRunning(a) && a.Incarnation == 2 }, effect: func(e *providerEffects) {
			e.stops++
			e.deletes++
			e.creates++
			e.starts++
			e.bootstraps++
		}},
	} {
		t.Run(action, func(t *testing.T) {
			w := runningUnsentWorker(t)
			w.fake(func(f *managedFakePVE) { f.stopReadFailures = 2 })
			effects := providerEffectsOf(w.pve)
			if ok, text, _ := w.call(t, action, ""); !ok {
				t.Fatalf("%s: %s", action, text)
			}
			waiting := w.get(t)
			if !stopWaiting(waiting) || waiting.OperationID == w.actor.OperationID {
				t.Fatalf("%s after a read failure before any write: %+v", action, waiting)
			}
			if phase, state := actorState(t, w.ts, w.alice, w.actor.SwarmID); phase != "running" || state != tc.state {
				t.Fatalf("API of a waiting %s: phase %v, state %v, want running, %s", action, phase, state, tc.state)
			}
			// Второй отказ до записи — то же ожидание.
			if again := passActor(t, w.ts, w.actor.UID); !stopWaiting(again) || again.OperationID != waiting.OperationID {
				t.Fatalf("retried stop after another read failure: %+v", again)
			}
			if got := providerEffectsOf(w.pve); got != effects {
				t.Fatalf("stop reached the provider before the read succeeded: %+v want %+v", got, effects)
			}
			settled := passUntil(t, w.ts, w.actor.UID, 10, tc.done)
			// Новое поколение reset запускает свой start, остальные
			// заканчивают то же намерение.
			if action != "reset" && settled.OperationID != waiting.OperationID {
				t.Fatalf("retried stop replaced the intent: %+v", settled)
			}
			want := effects
			tc.effect(&want)
			if got := providerEffectsOf(w.pve); got != want {
				t.Fatalf("%s effects: %+v want %+v", action, got, want)
			}
		})
	}
}

// TestManagedWaitingStopRefusesOtherChanges: пока Stop не запрошен, другие
// изменения жизненного цикла получают отказ, называющий ожидание и
// последний отказ, а не неизвестный исход; повторный stop — no-op.
func TestManagedWaitingStopRefusesOtherChanges(t *testing.T) {
	w := runningUnsentWorker(t)
	w.fake(func(f *managedFakePVE) { f.stopReadFailures = 1 })
	if ok, text, _ := w.call(t, "stop", ""); !ok {
		t.Fatalf("stop: %s", text)
	}
	waiting := w.get(t)
	effects := providerEffectsOf(w.pve)
	says := `actor "worker" is waiting to stop its running container (last attempt failed: Proxmox reconciliation unavailable; allocation retained); reconcile retries the stop on every pass`
	for _, change := range []string{"start", "delete", "reset"} {
		if ok, text, _ := w.call(t, change, ""); ok || !strings.Contains(refusalText(text), change+" refused: "+says) {
			t.Errorf("%s while the stop is waiting: %v %s", change, ok, text)
		}
	}
	if ok, text, _ := w.call(t, "stop", ""); !ok {
		t.Errorf("repeated stop while the stop is waiting: %s", text)
	}
	expectUnchanged(t, w.ts, w.pve, waiting, effects)
}

// roleWorker — работающий worker роя, его PVE и запрос изменения, первая
// стадия которого — stop.
type roleWorker struct {
	ts      *substrateServer
	alice   *http.Client
	worker  store.ManagedActor
	pve     *managedFakePVE
	request func(*testing.T)
}

// TestManagedWaitingStopTakesNoRole: актор, чью работающую CT reset или
// update ещё только должны остановить (Stop отказал до записи), для роя не
// работает: роль не разрешается и не привязывается, отказ называет ждущую
// остановку, а API актора показывает остановку, а не running. Когда
// изменение закончилось, update возвращает роль тому же актору, а у reset
// она устаревает вместе с инкарнацией.
func TestManagedWaitingStopTakesNoRole(t *testing.T) {
	for change, tc := range map[string]struct {
		setup func(*testing.T) roleWorker
		after error
	}{
		"update": {setup: func(t *testing.T) roleWorker {
			ts, alice, master, worker, pve, _ := updateSetup(t)
			guestReleaseHub(ts, reportsRelease(oldRelease))
			return roleWorker{ts, alice, worker, pve, func(t *testing.T) {
				if got := operatorUpdate(ts, alice, master.SwarmID)(t, "worker"); !got.ok || got.outcome != "accepted" {
					t.Fatalf("update: %+v", got)
				}
			}}
		}},
		"reset": {setup: func(t *testing.T) roleWorker {
			w := runningUnsentWorker(t)
			return roleWorker{w.ts, w.alice, w.actor, w.pve, func(t *testing.T) {
				if ok, text, _ := w.call(t, "reset", ""); !ok {
					t.Fatalf("reset: %s", text)
				}
			}}
		}, after: store.ErrSwarmRoleStale},
	} {
		t.Run(change, func(t *testing.T) {
			w := tc.setup(t)
			ctx := context.Background()
			s, swarm := w.ts.srv.store, w.worker.SwarmID
			if err := s.BindSwarmRole(ctx, store.SwarmRoleBinding{SwarmID: swarm, Role: "build", ActorUID: w.worker.UID}); err != nil {
				t.Fatal(err)
			}
			w.pve.mu.Lock()
			w.pve.stopReadFailures = 1
			w.pve.mu.Unlock()
			w.request(t)
			if a, err := s.GetManagedActor(ctx, w.worker.UID); err != nil || !stopWaiting(a) {
				t.Fatalf("%s stop not waiting: %+v %v", change, a, err)
			}
			says := "actor is waiting to stop its running container for " + change + " (last attempt failed: Proxmox reconciliation unavailable; allocation retained)"
			if _, err := s.ResolveSwarmRole(ctx, swarm, "build"); !errors.Is(err, store.ErrSwarmRoleStopPending) || !strings.Contains(fmt.Sprint(err), says) {
				t.Fatalf("role of an actor waiting to stop: %v, want %q", err, says)
			}
			r, b, _ := w.ts.do(w.alice, "PUT", "/api/swarms/"+swarm+"/roles/review", w.ts.csrf(w.alice), map[string]any{"actorUid": w.worker.UID})
			expectRoleDetail(t, "bind of an actor waiting to stop", r, b, http.StatusConflict, swarmRoleStopPending)
			if phase, state := actorState(t, w.ts, w.alice, swarm); phase != "running" || state != "ACTOR_STATE_SUSPENDING" {
				t.Fatalf("API of an actor waiting to stop: phase %v, state %v", phase, state)
			}
			passUntil(t, w.ts, w.worker.UID, 10, actorRunning)
			if _, err := s.ResolveSwarmRole(ctx, swarm, "build"); !errors.Is(err, tc.after) {
				t.Fatalf("role after the %s: %v, want %v", change, err, tc.after)
			}
		})
	}
}

// TestManagedWaitingStopOfStoppedContainerSendsNoStop: Stop ждёт запроса, а
// CT тем временем остановилась мимо brood. Проход не шлёт ей vzstop: PVE
// отвечает на него HTTP 500 «CT not running», и клиент считает исход
// неизвестным. Наблюдённая остановленная CT и есть подтверждённая
// остановка: stop кончается, delete и reset переходят к удалению.
func TestManagedWaitingStopOfStoppedContainerSendsNoStop(t *testing.T) {
	for action, tc := range map[string]struct {
		done   func(store.ManagedActor) bool
		effect func(*providerEffects)
	}{
		"stop":   {done: actorStopped, effect: func(*providerEffects) {}},
		"delete": {done: actorDeleted, effect: func(e *providerEffects) { e.deletes++ }},
		"reset": {done: func(a store.ManagedActor) bool { return actorRunning(a) && a.Incarnation == 2 }, effect: func(e *providerEffects) {
			e.deletes++
			e.creates++
			e.starts++
			e.bootstraps++
		}},
	} {
		t.Run(action, func(t *testing.T) {
			w := runningUnsentWorker(t)
			w.fake(func(f *managedFakePVE) { f.stopReadFailures = 1 })
			if ok, text, _ := w.call(t, action, ""); !ok {
				t.Fatalf("%s: %s", action, text)
			}
			waiting := w.get(t)
			if !stopWaiting(waiting) {
				t.Fatalf("%s stop not waiting: %+v", action, waiting)
			}
			w.fake(func(f *managedFakePVE) { f.running, f.stopRefusesStopped = false, true })
			effects := providerEffectsOf(w.pve)
			// Первый же проход подтверждает остановку, не оставляя стадию stop.
			if a := passActor(t, w.ts, w.actor.UID); a.Stage == "stop" || a.Phase == "error" || a.OperationID != waiting.OperationID {
				t.Fatalf("waiting %s of a stopped container: %+v", action, a)
			}
			passUntil(t, w.ts, w.actor.UID, 8, tc.done)
			want := effects
			tc.effect(&want)
			if got := providerEffectsOf(w.pve); got != want {
				t.Fatalf("%s effects: %+v want %+v", action, got, want)
			}
		})
	}
}

// TestManagedWaitingStageSurvivesObservationFailure: проход, чьё наблюдение CT
// отказало, пока start или stop ждут запроса, оставляет их ждать с этой
// причиной, а не делает исход неизвестным; следующий проход их отправляет.
func TestManagedWaitingStageSurvivesObservationFailure(t *testing.T) {
	for stage, tc := range map[string]struct {
		into    func(*testing.T, unsentWorker)
		waiting func(store.ManagedActor) bool
		done    func(store.ManagedActor) bool
	}{
		"start": {into: func(t *testing.T, w unsentWorker) {
			w.stopped(t)
			w.fake(func(f *managedFakePVE) { f.bootstrapReadFailures = 1 })
			w.call(t, "start", "")
		}, waiting: startWaiting, done: actorRunning},
		"stop": {into: func(t *testing.T, w unsentWorker) {
			w.fake(func(f *managedFakePVE) { f.stopReadFailures = 1 })
			w.call(t, "stop", "")
		}, waiting: stopWaiting, done: actorStopped},
	} {
		t.Run(stage, func(t *testing.T) {
			w := runningUnsentWorker(t)
			tc.into(t, w)
			if a := w.get(t); !tc.waiting(a) {
				t.Fatalf("%s not waiting: %+v", stage, a)
			}
			w.fake(func(f *managedFakePVE) { f.inspectError = proxmox.ErrPermission })
			if a := passActor(t, w.ts, w.actor.UID); !tc.waiting(a) || a.Error != managedProviderError(proxmox.ErrPermission) {
				t.Fatalf("%s after an observation failure: %+v", stage, a)
			}
			w.fake(func(f *managedFakePVE) { f.inspectError = nil })
			passUntil(t, w.ts, w.actor.UID, 4, tc.done)
		})
	}
}

// TestManagedAmbiguousStopIsNotRetried: Stop, который PVE мог получить
// (потерянный ответ, незаписанный UPID), остаётся неизвестным исходом и
// второй раз не уходит, даже если CT пока работает. Так и у повтора
// ожидавшего Stop: перед отправкой стадия отмечена stopping.
func TestManagedAmbiguousStopIsNotRetried(t *testing.T) {
	queued := func(w unsentWorker) func() {
		return func() {
			w.pve.mu.Lock()
			w.pve.running = true
			w.pve.mu.Unlock()
		}
	}
	for name, fault := range map[string]func(*testing.T, unsentWorker){
		"stop reply lost": func(_ *testing.T, w unsentWorker) {
			w.fake(func(f *managedFakePVE) { f.uncertainStop, f.afterStop = true, queued(w) })
		},
		"stop task unrecorded": func(t *testing.T, w unsentWorker) {
			mark := queued(w)
			w.fake(func(f *managedFakePVE) {
				f.afterStop = func() {
					mark()
					execDB(t, w.ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, w.actor.UID)
				}
			})
		},
	} {
		for _, retried := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "", true: " on retry"}[retried], func(t *testing.T) {
				w := runningUnsentWorker(t)
				effects := providerEffectsOf(w.pve)
				if retried {
					w.fake(func(f *managedFakePVE) { f.stopReadFailures = 1 })
					w.call(t, "stop", "")
					if a := w.get(t); !stopWaiting(a) {
						t.Fatalf("stop not waiting: %+v", a)
					}
					fault(t, w)
				} else {
					fault(t, w)
					w.call(t, "stop", "")
				}
				var a store.ManagedActor
				for range 5 {
					a = passActor(t, w.ts, w.actor.UID)
				}
				if a.Stage != "stop" || a.UPID != "" || a.Phase != "error" || a.Error != managedProviderError(proxmox.ErrUncertain) {
					t.Fatalf("ambiguous stop: %+v", a)
				}
				want := effects
				want.stops++
				if got := providerEffectsOf(w.pve); got != want {
					t.Fatalf("ambiguous stop effects: %+v want %+v", got, want)
				}
			})
		}
	}
}

// TestManagedStartRetriesFailureBeforeWrite: обычный start (resume
// остановленного актора) после отказа до Start — чтения bootstrap или Start,
// неоднозначной записи bootstrap — ждёт, а не становится неизвестным, и
// следующий проход запускает CT один раз. Ожидающий start отказывает
// повторному start, называя ожидание; stop отменяет его без vzstop и без
// Start.
func TestManagedStartRetriesFailureBeforeWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		fail       func(*managedFakePVE)
		bootstraps int
	}{
		"bootstrap read": {fail: func(f *managedFakePVE) { f.bootstrapReadFailures = 1 }, bootstraps: 1},
		"start read":     {fail: func(f *managedFakePVE) { f.startReadFailures = 1 }, bootstraps: 1},
		"bootstrap 5xx":  {fail: func(f *managedFakePVE) { f.bootstrapUncertain = 1 }, bootstraps: 2},
	} {
		for _, cancel := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "", true: ", then stop"}[cancel], func(t *testing.T) {
				w := runningUnsentWorker(t)
				stopped := w.stopped(t)
				w.fake(tc.fail)
				effects := providerEffectsOf(w.pve)
				if ok, text, _ := w.call(t, "start", ""); !ok {
					t.Fatalf("start: %s", text)
				}
				waiting := w.get(t)
				if !startWaiting(waiting) || waiting.OperationID == stopped.OperationID {
					t.Fatalf("start after a failure before Start: %+v", waiting)
				}
				if got := providerEffectsOf(w.pve); got.starts != effects.starts {
					t.Fatalf("start reached the provider: %+v", got)
				}
				if cancel {
					if ok, text, _ := w.call(t, "start", ""); ok || !strings.Contains(refusalText(text), `start refused: actor "worker" is waiting to start its stopped container (last attempt failed: `) {
						t.Fatalf("start while the start is waiting: %v %s", ok, text)
					}
					after := providerEffectsOf(w.pve)
					if ok, text, _ := w.call(t, "stop", ""); !ok {
						t.Fatalf("stop of a waiting start: %s", text)
					}
					for range 3 {
						if a := passActor(t, w.ts, w.actor.UID); !actorStopped(a) || a.Operation != "stop" {
							t.Fatalf("canceled start: %+v", a)
						}
					}
					if got := providerEffectsOf(w.pve); got != after {
						t.Fatalf("canceled start reached the provider: %+v want %+v", got, after)
					}
					return
				}
				restarted := passUntil(t, w.ts, w.actor.UID, 6, actorRunning)
				if restarted.OperationID != waiting.OperationID {
					t.Fatalf("retried start replaced the intent: %+v", restarted)
				}
				want := effects
				want.starts++
				want.bootstraps += tc.bootstraps
				if got := providerEffectsOf(w.pve); got != want {
					t.Fatalf("start effects: %+v want %+v", got, want)
				}
			})
		}
	}
}

// TestManagedStartNeverStartsTwice: обычный start, чей Start PVE мог
// получить (потерянный ответ, незаписанный UPID), остаётся неизвестным
// исходом, даже если CT пока остановлена: Start второй раз не уходит.
func TestManagedStartNeverStartsTwice(t *testing.T) {
	stillStopped := func(w unsentWorker) func() {
		return func() {
			w.pve.mu.Lock()
			w.pve.running = false
			w.pve.mu.Unlock()
		}
	}
	for name, fault := range map[string]func(*testing.T, unsentWorker){
		"start reply lost": func(_ *testing.T, w unsentWorker) {
			w.fake(func(f *managedFakePVE) { f.uncertainStart = true })
		},
		"start reply lost, container still stopped": func(_ *testing.T, w unsentWorker) {
			w.fake(func(f *managedFakePVE) { f.uncertainStart, f.afterStart = true, stillStopped(w) })
		},
		"start task unrecorded": func(t *testing.T, w unsentWorker) {
			stop := stillStopped(w)
			w.fake(func(f *managedFakePVE) {
				f.afterStart = func() {
					stop()
					execDB(t, w.ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, w.actor.UID)
				}
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := runningUnsentWorker(t)
			w.stopped(t)
			fault(t, w)
			effects := providerEffectsOf(w.pve)
			w.call(t, "start", "")
			var a store.ManagedActor
			for range 6 {
				a = passActor(t, w.ts, w.actor.UID)
			}
			if !startPending(a) || a.Phase != "error" || a.Error != managedProviderError(proxmox.ErrUncertain) {
				t.Fatalf("unknown start outcome: %+v", a)
			}
			want := effects
			want.starts++
			want.bootstraps++
			if got := providerEffectsOf(w.pve); got != want {
				t.Fatalf("start effects: %+v want %+v", got, want)
			}
		})
	}
}

// TestManagedStopOfStoppedContainerSendsNoStop: stop и reset актора, чья CT
// уже остановлена, не шлют vzstop: настоящий PVE отвечает на него HTTP 500
// «CT not running», и клиент считает исход неизвестным. Остановленная CT,
// проверенная наблюдением, и есть подтверждённая остановка.
func TestManagedStopOfStoppedContainerSendsNoStop(t *testing.T) {
	for name, tc := range map[string]struct {
		into   func(*testing.T, unsentWorker)
		action string
		done   func(store.ManagedActor) bool
		effect func(*providerEffects)
	}{
		// Start-задача упала, CT осталась остановленной.
		"stop after a failed start": {into: func(t *testing.T, w unsentWorker) {
			w.stopped(t)
			w.fake(func(f *managedFakePVE) {
				f.taskErrors = map[string]error{"start-task": proxmox.ErrTaskFailed}
				f.afterStart = func() {
					w.pve.mu.Lock()
					w.pve.running = false
					w.pve.mu.Unlock()
				}
			})
			if ok, text, _ := w.call(t, "start", ""); !ok {
				t.Fatalf("start: %s", text)
			}
			if a := passActor(t, w.ts, w.actor.UID); a.Stage != "done" || a.Phase != "error" || a.Desired != "running" {
				t.Fatalf("failed start task: %+v", a)
			}
			w.fake(func(f *managedFakePVE) { f.taskErrors, f.afterStart = nil, nil })
		}, action: "stop", done: actorStopped},
		"stop of a container stopped outside brood": {into: func(_ *testing.T, w unsentWorker) {
			w.fake(func(f *managedFakePVE) { f.running = false })
		}, action: "stop", done: actorStopped},
		"reset of a stopped actor": {into: func(t *testing.T, w unsentWorker) { w.stopped(t) }, action: "reset", done: func(a store.ManagedActor) bool {
			return actorRunning(a) && a.Incarnation == 2
		}, effect: func(e *providerEffects) {
			e.deletes++
			e.creates++
			e.starts++
			e.bootstraps++
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := runningUnsentWorker(t)
			tc.into(t, w)
			w.fake(func(f *managedFakePVE) { f.stopRefusesStopped = true })
			effects := providerEffectsOf(w.pve)
			if ok, text, _ := w.call(t, tc.action, ""); !ok {
				t.Fatalf("%s: %s", tc.action, text)
			}
			if tc.action == "stop" {
				// Остановка подтверждена сразу, задачи нет.
				if a := w.get(t); !actorStopped(a) || a.Operation != "stop" || a.Error != "" {
					t.Fatalf("stop of a stopped container: %+v", a)
				}
			}
			passUntil(t, w.ts, w.actor.UID, 8, tc.done)
			want := effects
			if tc.effect != nil {
				tc.effect(&want)
			}
			if got := providerEffectsOf(w.pve); got != want {
				t.Fatalf("%s effects: %+v want %+v", tc.action, got, want)
			}
		})
	}
}

// TestUpdateStopRetriesFailureBeforeWrite: update, чей Stop отказал на
// чтении владения, принят: намерение записано, PVE Stop не получал, и
// reconcile запрашивает его каждым проходом. Это не неизвестный исход.
func TestUpdateStopRetriesFailureBeforeWrite(t *testing.T) {
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			ts, alice, master, worker, pve, target := updateSetup(t)
			guestReleaseHub(ts, reportsRelease(oldRelease))
			pve.mu.Lock()
			pve.stopReadFailures = 1
			pve.mu.Unlock()
			effects := providerEffectsOf(pve)
			got := updateCallers(ts, alice, master)[via](t, "worker")
			if !got.ok || got.outcome != "accepted" || strings.Contains(got.text, "outcome is unknown") || !strings.Contains(got.text, "from guest release "+oldRelease+" to "+target) || !strings.Contains(got.text, "its stop is not yet requested (last attempt failed: Proxmox reconciliation unavailable; allocation retained); reconcile requests it on every pass") {
				t.Fatalf("update after a stop read failure: %+v", got)
			}
			intent, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
			if err != nil || !stopWaiting(intent) || intent.Operation != "start" || intent.Desired != "running" {
				t.Fatalf("update intent: %+v %v", intent, err)
			}
			if got := providerEffectsOf(pve); got != effects {
				t.Fatalf("update stop reached the provider: %+v want %+v", got, effects)
			}
			restarted := passUntil(t, ts, worker.UID, 8, actorRunning)
			if restarted.OperationID != intent.OperationID || restarted.Incarnation != worker.Incarnation {
				t.Fatalf("update restart: %+v", restarted)
			}
			want := effects
			want.stops++
			want.starts++
			want.bootstraps++
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("update effects: %+v want %+v", got, want)
			}
		})
	}
}
