package server

// BUG: Незапрошенные delete и stop застревают; незапрошенный start
// становится неизвестным после внешнего запуска CT (#277).
// HYPOTHESIS: Записанное намерение не отличает отправку от сбоя до неё,
// а наблюдение работающей CT ошибочно считается неоднозначным start.
// SOLUTION: Stop- и delete-стадии, чью запись PVE не получал, несут отметку
// незапрошенной стадии (store.ManagedStopUnsent, store.ManagedDeleteUnsent);
// её ставят только переходы стадий, и перед отправкой её снимают, так что
// потерянный ответ и незаписанный UPID второй раз запись не шлют. Стадия
// без отметки, в том числе записанная до #277, остаётся неизвестной.
// Ожидающий start принимает CT, запущенную мимо brood, после проверки
// владения; ожидающий delete такой CT сначала снова её останавливает.
// Отсутствие CT закрывает стадию и освобождает аллокацию одной
// транзакцией. Отметка — состояние, а не отказ: API, отказы и ответ update
// показывают только диагностику.
// RESULT: FIXED
// STATUS: FIXED

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// deleteWaiting — стадия delete, чей Delete PVE ещё не получал: CT
// остановлена, задачи нет, отметка незапрошенного Delete на месте.
func deleteWaiting(a store.ManagedActor) bool {
	return a.Stage == "delete" && a.UPID == "" && a.Phase == "stopped" && a.Unsent() == store.ManagedDeleteUnsent
}

// waitingDelete — остановленный worker, чей delete или reset ждёт запроса
// Delete: первый Delete отказал на чтении владения, до записи.
func waitingDelete(t *testing.T, action string) unsentWorker {
	t.Helper()
	w := runningUnsentWorker(t)
	w.stopped(t)
	w.fake(func(f *managedFakePVE) { f.deleteReadFailures = 1 })
	if ok, text, _ := w.call(t, action, ""); !ok {
		t.Fatalf("%s: %s", action, text)
	}
	if a := w.get(t); !deleteWaiting(a) {
		t.Fatalf("%s delete not waiting: %+v", action, a)
	}
	return w
}

// actorStatus — status актора worker в API.
func actorStatus(t *testing.T, w unsentWorker) map[string]any {
	t.Helper()
	r, b, _ := w.ts.do(w.alice, "GET", "/api/swarms/"+w.actor.SwarmID+"/actors/worker", "", nil)
	expectStatus(t, "read worker", r, b, http.StatusOK)
	status, _ := b["status"].(map[string]any)
	return status
}

// resetDone — reset закончен новой инкарнацией, которая работает.
func resetDone(a store.ManagedActor) bool { return actorRunning(a) && a.Incarnation == 2 }

// recreated — эффекты reset после удаления: новая CT создана и запущена.
func recreated(e *providerEffects) {
	e.creates++
	e.starts++
	e.bootstraps++
}

func Test277DeleteRetriesUnsent(t *testing.T) {
	w := runningUnsentWorker(t)
	w.stopped(t)
	w.fake(func(f *managedFakePVE) { f.deleteReadFailures = 1 })
	before := providerEffectsOf(w.pve)
	if ok, text, _ := w.call(t, "delete", ""); !ok {
		t.Fatalf("delete: %s", text)
	}
	passUntil(t, w.ts, w.actor.UID, 6, actorDeleted)
	want := before
	want.deletes++
	if got := providerEffectsOf(w.pve); got != want {
		t.Fatalf("effects: %+v want %+v", got, want)
	}
}

// Test277AdoptsExternallyStartedContainer: ожидающий start, чью CT
// запустили мимо brood, ничего не пишет в PVE и принимает CT; running
// публикует только проверка готовности гостя.
func Test277AdoptsExternallyStartedContainer(t *testing.T) {
	w := runningUnsentWorker(t)
	w.stopped(t)
	w.fake(func(f *managedFakePVE) { f.bootstrapReadFailures = 1 })
	if ok, text, _ := w.call(t, "start", ""); !ok {
		t.Fatalf("start: %s", text)
	}
	if a := w.get(t); !startWaiting(a) {
		t.Fatalf("start not waiting: %+v", a)
	}
	w.fake(func(f *managedFakePVE) { f.running = true })
	w.guestReady(false)
	before := providerEffectsOf(w.pve)
	for range 3 {
		if a := passActor(t, w.ts, w.actor.UID); a.Stage != "done" || a.Phase != "provisioning" || a.Error != "" {
			t.Fatalf("adopted container before guest readiness: %+v", a)
		}
	}
	w.guestReady(true)
	passUntil(t, w.ts, w.actor.UID, 4, actorRunning)
	if got := providerEffectsOf(w.pve); got != before {
		t.Fatalf("unexpected provider writes: %+v want %+v", got, before)
	}
}

// Test277LegacyStageIsNeverResent: стадия без отметки незапрошенной записи
// могла быть оставлена прежним сервером после отправки и до сохранения UPID,
// даже если CT ещё существует или работает: delete-стадия в фазе stopped и
// stop-стадия, начатая до #277 (фаза stopping без отметки). Её исход
// неизвестен, и запись не повторяется.
func Test277LegacyStageIsNeverResent(t *testing.T) {
	for stage, into := range map[string]func(*testing.T, unsentWorker){
		"delete": func(t *testing.T, w unsentWorker) {
			w.stopped(t)
			w.fake(func(f *managedFakePVE) { f.deleteReadFailures = 1 })
			if ok, text, _ := w.call(t, "delete", ""); !ok {
				t.Fatalf("delete: %s", text)
			}
		},
		"stop": func(t *testing.T, w unsentWorker) {
			if _, err := w.ts.srv.store.BeginManagedActorOperation(context.Background(), w.actor.UID, w.actor.Revision, store.NewID(), "stop"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(stage, func(t *testing.T) {
			w := runningUnsentWorker(t)
			into(t, w)
			execDB(t, w.ts, `UPDATE managed_actors SET error='' WHERE uid=?`, w.actor.UID)
			before := providerEffectsOf(w.pve)
			for range 3 {
				a := passActor(t, w.ts, w.actor.UID)
				if a.Stage != stage || a.UPID != "" || a.Error != managedProviderError(proxmox.ErrUncertain) {
					t.Fatalf("legacy %s: %+v", stage, a)
				}
			}
			if got := providerEffectsOf(w.pve); got != before {
				t.Fatalf("legacy %s resent: %+v want %+v", stage, got, before)
			}
		})
	}
}

// Test277StopSurvivesCrashBeforeRequest: сервер остановился между записью
// намерения и отправкой Stop. Stop-стадия любой операции отмечена
// незапрошенной, и следующий проход отправляет её один раз.
func Test277StopSurvivesCrashBeforeRequest(t *testing.T) {
	for action, tc := range map[string]struct {
		done   func(store.ManagedActor) bool
		effect func(*providerEffects)
	}{
		"stop":   {done: actorStopped, effect: func(e *providerEffects) { e.stops++ }},
		"delete": {done: actorDeleted, effect: func(e *providerEffects) { e.stops++; e.deletes++ }},
		"reset":  {done: resetDone, effect: func(e *providerEffects) { e.stops++; e.deletes++; recreated(e) }},
		"update": {done: actorRunning, effect: func(e *providerEffects) { e.stops++; e.starts++; e.bootstraps++ }},
	} {
		t.Run(action, func(t *testing.T) {
			w := runningUnsentWorker(t)
			before := providerEffectsOf(w.pve)
			a, err := w.ts.srv.store.BeginManagedActorOperation(context.Background(), w.actor.UID, w.actor.Revision, store.NewID(), action)
			if err != nil {
				t.Fatal(err)
			}
			if a.Stage != "stop" || a.UPID != "" || a.Unsent() != store.ManagedStopUnsent {
				t.Fatalf("unsent %s intent: %+v", action, a)
			}
			passUntil(t, w.ts, w.actor.UID, 10, tc.done)
			want := before
			tc.effect(&want)
			if got := providerEffectsOf(w.pve); got != want {
				t.Fatalf("%s effects: %+v want %+v", action, got, want)
			}
		})
	}
}

// Test277AmbiguousDeleteIsNotRetried: Delete, который PVE мог получить
// (потерянный ответ, незаписанный UPID), остаётся неизвестным исходом и
// второй раз не уходит, даже если CT ещё есть: её удаление стоит в очереди
// задач. Так и у повтора ожидавшего Delete: перед отправкой отметка снята.
func Test277AmbiguousDeleteIsNotRetried(t *testing.T) {
	queued := func(w unsentWorker) func() {
		return func() {
			w.pve.mu.Lock()
			w.pve.exists = true
			w.pve.mu.Unlock()
		}
	}
	for name, fault := range map[string]func(*testing.T, unsentWorker){
		"delete reply lost": func(_ *testing.T, w unsentWorker) {
			w.fake(func(f *managedFakePVE) { f.uncertainDelete, f.afterDelete = true, queued(w) })
		},
		"delete task unrecorded": func(t *testing.T, w unsentWorker) {
			mark := queued(w)
			w.fake(func(f *managedFakePVE) {
				f.afterDelete = func() {
					mark()
					execDB(t, w.ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, w.actor.UID)
				}
			})
		},
	} {
		for _, retried := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "", true: " on retry"}[retried], func(t *testing.T) {
				w := runningUnsentWorker(t)
				w.stopped(t)
				effects := providerEffectsOf(w.pve)
				if retried {
					w.fake(func(f *managedFakePVE) { f.deleteReadFailures = 1 })
					w.call(t, "delete", "")
					if a := w.get(t); !deleteWaiting(a) {
						t.Fatalf("delete not waiting: %+v", a)
					}
					fault(t, w)
				} else {
					fault(t, w)
					w.call(t, "delete", "")
				}
				var a store.ManagedActor
				for range 5 {
					a = passActor(t, w.ts, w.actor.UID)
				}
				if a.Stage != "delete" || a.UPID != "" || a.Phase != "error" || a.Error != managedProviderError(proxmox.ErrUncertain) {
					t.Fatalf("ambiguous delete: %+v", a)
				}
				want := effects
				want.deletes++
				if got := providerEffectsOf(w.pve); got != want {
					t.Fatalf("ambiguous delete effects: %+v want %+v", got, want)
				}
			})
		}
	}
}

// Test277WaitingStageSurvivesObservationFailure: проход, чьё наблюдение CT
// отказало, пока Delete или Stop ещё не запрошены, оставляет стадию ждать с
// отметкой и новой причиной; следующий проход отправляет запись один раз.
func Test277WaitingStageSurvivesObservationFailure(t *testing.T) {
	begun := func(action string) func(*testing.T) unsentWorker {
		return func(t *testing.T) unsentWorker {
			w := runningUnsentWorker(t)
			// Сервер остановился до отправки Stop.
			if _, err := w.ts.srv.store.BeginManagedActorOperation(context.Background(), w.actor.UID, w.actor.Revision, store.NewID(), action); err != nil {
				t.Fatal(err)
			}
			return w
		}
	}
	for name, tc := range map[string]struct {
		into   func(*testing.T) unsentWorker
		done   func(store.ManagedActor) bool
		effect func(*providerEffects)
	}{
		"waiting delete": {into: func(t *testing.T) unsentWorker { return waitingDelete(t, "delete") }, done: actorDeleted, effect: func(e *providerEffects) { e.deletes++ }},
		"waiting reset":  {into: func(t *testing.T) unsentWorker { return waitingDelete(t, "reset") }, done: resetDone, effect: func(e *providerEffects) { e.deletes++; recreated(e) }},
		"begun stop":     {into: begun("stop"), done: actorStopped, effect: func(e *providerEffects) { e.stops++ }},
		"begun delete":   {into: begun("delete"), done: actorDeleted, effect: func(e *providerEffects) { e.stops++; e.deletes++ }},
	} {
		t.Run(name, func(t *testing.T) {
			w := tc.into(t)
			waiting := w.get(t)
			effects := providerEffectsOf(w.pve)
			w.fake(func(f *managedFakePVE) { f.inspectError = proxmox.ErrPermission })
			a := passActor(t, w.ts, w.actor.UID)
			if a.Stage != waiting.Stage || a.Phase != waiting.Phase || a.Unsent() == "" || a.Unsent() != waiting.Unsent() || a.Diagnostic() != managedProviderError(proxmox.ErrPermission) {
				t.Fatalf("%s after an observation failure: %+v", name, a)
			}
			w.fake(func(f *managedFakePVE) { f.inspectError = nil })
			passUntil(t, w.ts, w.actor.UID, 10, tc.done)
			want := effects
			tc.effect(&want)
			if got := providerEffectsOf(w.pve); got != want {
				t.Fatalf("%s effects: %+v want %+v", name, got, want)
			}
		})
	}
}

// Test277WaitingDeleteRefusesOtherChanges: пока Delete не запрошен, другие
// изменения, в том числе update, получают отказ, называющий ожидание
// удаления и последний отказ, без совета отменить его. No-op — только повтор
// того же намерения: delete при ждущем delete. Ждущее удаление reset ведёт
// другое намерение, и delete получает тот же отказ.
func Test277WaitingDeleteRefusesOtherChanges(t *testing.T) {
	says := `actor "worker" is waiting to delete its stopped container (last attempt failed: Proxmox reconciliation unavailable; allocation retained); reconcile retries the delete on every pass`
	refused := func(t *testing.T, change string, ok bool, reason string) {
		t.Helper()
		if ok || !strings.Contains(reason, change+" refused: "+says) || strings.Contains(reason, "cancels") || strings.Contains(reason, store.ManagedDeleteUnsent) {
			t.Errorf("%s while the delete is waiting: %v %s", change, ok, reason)
		}
	}
	for action, changes := range map[string][]string{
		"delete": {"start", "stop", "reset"},
		"reset":  {"start", "stop", "reset", "delete"},
	} {
		t.Run(action, func(t *testing.T) {
			w := waitingDelete(t, action)
			waiting := w.get(t)
			effects := providerEffectsOf(w.pve)
			for _, change := range changes {
				ok, text, _ := w.call(t, change, "")
				refused(t, change, ok, refusalText(text))
			}
			got := operatorUpdate(w.ts, w.alice, w.actor.SwarmID)(t, "worker")
			refused(t, "update", got.ok, got.text)
			if action == "delete" {
				if ok, text, _ := w.call(t, "delete", ""); !ok {
					t.Errorf("repeated delete while the delete is waiting: %s", text)
				}
			}
			expectUnchanged(t, w.ts, w.pve, waiting, effects)
		})
	}
}

// Test277UnsentMarkIsNotShown: отметка незапрошенной стадии — состояние, а
// не отказ. API показывает только диагностику, а отказ называет запись ещё
// не запрошенной, а не «последнюю неудачу» с текстом отметки.
func Test277UnsentMarkIsNotShown(t *testing.T) {
	t.Run("begun stop", func(t *testing.T) {
		w := runningUnsentWorker(t)
		if _, err := w.ts.srv.store.BeginManagedActorOperation(context.Background(), w.actor.UID, w.actor.Revision, store.NewID(), "stop"); err != nil {
			t.Fatal(err)
		}
		if status := actorStatus(t, w); status["errorMessage"] != "" || status["state"] != "ACTOR_STATE_SUSPENDING" {
			t.Fatalf("API of a stop not yet requested: %v", status)
		}
		ok, text, _ := w.call(t, "start", "")
		if reason := refusalText(text); ok || !strings.Contains(reason, `start refused: actor "worker" is waiting to stop its container (Stop is not yet requested); reconcile retries the stop on every pass`) {
			t.Fatalf("start while the stop is not yet requested: %v %s", ok, reason)
		}
	})
	t.Run("waiting delete", func(t *testing.T) {
		w := waitingDelete(t, "delete")
		if status := actorStatus(t, w); status["errorMessage"] != "Proxmox reconciliation unavailable; allocation retained" {
			t.Fatalf("API of a waiting delete: %v", status)
		}
	})
}

// Test277DeletePhaseFollowsOperation: фаза запрошенного Delete — deleting у
// delete и stopped у reset, как до #277: старой инкарнации reset ответ на
// вопрос доходит, пока её CT удаляют. Ожидающий Delete показывает тот же
// status.state, что запрошенный: удаление у delete.
func Test277DeletePhaseFollowsOperation(t *testing.T) {
	for action, tc := range map[string]struct {
		phase, state string
		done         func(store.ManagedActor) bool
	}{
		"delete": {phase: "deleting", state: "ACTOR_STATE_DELETING", done: actorDeleted},
		"reset":  {phase: "stopped", state: "ACTOR_STATE_UNSPECIFIED", done: resetDone},
	} {
		t.Run(action, func(t *testing.T) {
			w := waitingDelete(t, action)
			if phase, state := actorState(t, w.ts, w.alice, w.actor.SwarmID); phase != "stopped" || state != tc.state {
				t.Fatalf("API of a waiting %s delete: phase %v, state %v, want stopped, %s", action, phase, state, tc.state)
			}
			var phases []string
			w.fake(func(f *managedFakePVE) {
				f.afterDelete = func() { phases = append(phases, w.get(t).Phase) }
			})
			passUntil(t, w.ts, w.actor.UID, 6, tc.done)
			if strings.Join(phases, ",") != tc.phase {
				t.Fatalf("%s phase while PVE took the Delete: %v, want %s", action, phases, tc.phase)
			}
		})
	}
}

// Test277AbsentCleanupReleasesAtomically: очистка, нашедшая CT
// отсутствующей, закрывает стадию и освобождает аллокацию (у reset —
// начинает новую инкарнацию) одной транзакцией. Сервер, остановившийся
// посреди неё, не оставляет стадию delete без отметки: следующий проход
// снова находит CT отсутствующей и кончает очистку без записи в PVE.
func Test277AbsentCleanupReleasesAtomically(t *testing.T) {
	const deletedCrash = `CREATE TRIGGER crash BEFORE UPDATE OF phase ON managed_actors WHEN NEW.phase='deleted' BEGIN SELECT RAISE(ABORT, 'server stopped'); END`
	const resetCrash = `CREATE TRIGGER crash BEFORE UPDATE ON managed_actors WHEN NEW.incarnation<>OLD.incarnation BEGIN SELECT RAISE(ABORT, 'server stopped'); END`
	begun := func(action string) func(*testing.T, unsentWorker) {
		return func(t *testing.T, w unsentWorker) {
			if _, err := w.ts.srv.store.BeginManagedActorOperation(context.Background(), w.actor.UID, w.actor.Revision, store.NewID(), action); err != nil {
				t.Fatal(err)
			}
			w.fake(func(f *managedFakePVE) { f.exists, f.running = false, false })
			passActor(t, w.ts, w.actor.UID)
		}
	}
	for name, tc := range map[string]struct {
		crash   string
		stopped bool
		run     func(*testing.T, unsentWorker)
		done    func(store.ManagedActor) bool
		effect  func(*providerEffects)
	}{
		"delete pass": {crash: deletedCrash, run: begun("delete"), done: actorDeleted, effect: func(*providerEffects) {}},
		"reset pass":  {crash: resetCrash, run: begun("reset"), done: resetDone, effect: recreated},
		"delete request": {crash: deletedCrash, stopped: true, run: func(t *testing.T, w unsentWorker) {
			w.fake(func(f *managedFakePVE) { f.exists = false })
			w.call(t, "delete", "")
		}, done: actorDeleted, effect: func(*providerEffects) {}},
	} {
		t.Run(name, func(t *testing.T) {
			w := runningUnsentWorker(t)
			if tc.stopped {
				w.actor = w.stopped(t)
			}
			execDB(t, w.ts, tc.crash)
			effects := providerEffectsOf(w.pve)
			tc.run(t, w)
			if a := w.get(t); a.Stage == "delete" && !deleteWaiting(a) {
				t.Fatalf("%s left a delete stage that looks submitted: %+v", name, a)
			}
			execDB(t, w.ts, `DROP TRIGGER crash`)
			passUntil(t, w.ts, w.actor.UID, 6, tc.done)
			tc.effect(&effects)
			if got := providerEffectsOf(w.pve); got != effects {
				t.Fatalf("%s effects: %+v want %+v", name, got, effects)
			}
		})
	}
}

// Test277WaitingDeleteStopsExternallyStartedContainer: CT, чей Delete ещё
// не запрошен, запустили мимо brood. Delete работающей CT не уходит:
// стадия возвращается к незапрошенной остановке той же операции, проход
// останавливает CT одним Stop и потом удаляет её одним Delete. Пока Stop
// ждёт, API показывает работающую CT и идущую очистку.
func Test277WaitingDeleteStopsExternallyStartedContainer(t *testing.T) {
	for action, tc := range map[string]struct {
		state  string
		done   func(store.ManagedActor) bool
		effect func(*providerEffects)
	}{
		"delete": {state: "ACTOR_STATE_DELETING", done: actorDeleted, effect: func(e *providerEffects) { e.stops++; e.deletes++ }},
		"reset":  {state: "ACTOR_STATE_SUSPENDING", done: resetDone, effect: func(e *providerEffects) { e.stops++; e.deletes++; recreated(e) }},
	} {
		t.Run(action, func(t *testing.T) {
			w := waitingDelete(t, action)
			waiting := w.get(t)
			w.fake(func(f *managedFakePVE) { f.running, f.stopReadFailures = true, 1 })
			effects := providerEffectsOf(w.pve)
			a := passActor(t, w.ts, w.actor.UID)
			if !stopWaiting(a) || a.OperationID != waiting.OperationID {
				t.Fatalf("waiting %s of a running container: %+v", action, a)
			}
			if phase, state := actorState(t, w.ts, w.alice, w.actor.SwarmID); phase != "running" || state != tc.state {
				t.Fatalf("API of a %s stopping again: phase %v, state %v, want running, %s", action, phase, state, tc.state)
			}
			passUntil(t, w.ts, w.actor.UID, 10, tc.done)
			want := effects
			tc.effect(&want)
			if got := providerEffectsOf(w.pve); got != want {
				t.Fatalf("%s effects: %+v want %+v", action, got, want)
			}
			w.pve.mu.Lock()
			defer w.pve.mu.Unlock()
			if w.pve.deletedWhileRunning {
				t.Fatal("Delete reached a running container")
			}
		})
	}
}

// Test277UpdateAcceptsStopNotYetRequested: update, чьё намерение записано,
// а отметка отправки Stop не записалась (сервер остановился), Stop PVE не
// получал: reconcile его запросит. Ответ — принятие с незапрошенной
// остановкой, а не неизвестный исход «не повторяется само».
func Test277UpdateAcceptsStopNotYetRequested(t *testing.T) {
	ts, alice, master, worker, pve, target := updateSetup(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	execDB(t, ts, `CREATE TRIGGER crash BEFORE UPDATE ON managed_actors WHEN OLD.error='`+store.ManagedStopUnsent+`' AND NEW.error='' BEGIN SELECT RAISE(ABORT, 'server stopped'); END`)
	effects := providerEffectsOf(pve)
	got := operatorUpdate(ts, alice, master.SwarmID)(t, "worker")
	if !got.ok || got.outcome != "accepted" || strings.Contains(got.text, "not retried automatically") || strings.Contains(got.text, store.ManagedStopUnsent) || !strings.Contains(got.text, "from guest release "+oldRelease+" to "+target) || !strings.Contains(got.text, "its stop is not yet requested; reconcile requests it on every pass") {
		t.Fatalf("update whose stop is not yet requested: %+v", got)
	}
	execDB(t, ts, `DROP TRIGGER crash`)
	passUntil(t, ts, worker.UID, 8, actorRunning)
	effects.stops++
	effects.starts++
	effects.bootstraps++
	if got := providerEffectsOf(pve); got != effects {
		t.Fatalf("update effects: %+v want %+v", got, effects)
	}
}

// Test277DeleteOfContainerStartedMeanwhileIsUnknown: CT ждущего Delete
// запустили мимо brood уже после наблюдения прохода. PVE отвергает Delete
// работающей CT ответом HTTP 500, а клиент считает 500 неоднозначным: исход
// неизвестен, отметка снята перед отправкой, и ни Delete, ни Stop больше не
// уходят.
func Test277DeleteOfContainerStartedMeanwhileIsUnknown(t *testing.T) {
	w := waitingDelete(t, "delete")
	waiting := w.get(t)
	w.fake(func(f *managedFakePVE) {
		f.beforeDelete = func() {
			w.pve.mu.Lock()
			w.pve.running = true
			w.pve.mu.Unlock()
		}
	})
	effects := providerEffectsOf(w.pve)
	var a store.ManagedActor
	for range 4 {
		a = passActor(t, w.ts, w.actor.UID)
	}
	if a.Stage != "delete" || a.UPID != "" || a.Phase != "error" || a.Unsent() != "" || a.Error != managedProviderError(proxmox.ErrUncertain) || a.OperationID != waiting.OperationID {
		t.Fatalf("Delete refused for a running container: %+v", a)
	}
	effects.deletes++
	if got := providerEffectsOf(w.pve); got != effects {
		t.Fatalf("effects after the refused Delete: %+v want %+v", got, effects)
	}
}

// Test277LostStopAfterReopenedStopIsNotRetried: CT ждущего Delete запустили
// мимо brood, стадия вернулась к остановке, а ответ её Stop потерян (CT ещё
// работает: остановка стоит в очереди). Это неизвестный исход, как у любого
// Stop: ни второго Stop, ни Delete.
func Test277LostStopAfterReopenedStopIsNotRetried(t *testing.T) {
	for _, action := range []string{"delete", "reset"} {
		t.Run(action, func(t *testing.T) {
			w := waitingDelete(t, action)
			waiting := w.get(t)
			w.fake(func(f *managedFakePVE) {
				f.running, f.uncertainStop = true, true
				f.afterStop = func() {
					w.pve.mu.Lock()
					w.pve.running = true
					w.pve.mu.Unlock()
				}
			})
			effects := providerEffectsOf(w.pve)
			var a store.ManagedActor
			for range 5 {
				a = passActor(t, w.ts, w.actor.UID)
			}
			if a.Stage != "stop" || a.UPID != "" || a.Phase != "error" || a.Unsent() != "" || a.Error != managedProviderError(proxmox.ErrUncertain) || a.OperationID != waiting.OperationID {
				t.Fatalf("lost Stop of a reopened %s: %+v", action, a)
			}
			effects.stops++
			if got := providerEffectsOf(w.pve); got != effects {
				t.Fatalf("effects after the lost Stop: %+v want %+v", got, effects)
			}
		})
	}
}

// Test277WaitingDeleteOfAbsentContainer: CT ждущего Delete исчезла мимо
// brood. Очистка кончается без Delete: delete оставляет удалённую строку
// без отметки незапрошенной стадии, reset начинает новую инкарнацию.
func Test277WaitingDeleteOfAbsentContainer(t *testing.T) {
	for action, tc := range map[string]struct {
		done   func(store.ManagedActor) bool
		effect func(*providerEffects)
	}{
		"delete": {done: actorDeleted, effect: func(*providerEffects) {}},
		"reset":  {done: resetDone, effect: recreated},
	} {
		t.Run(action, func(t *testing.T) {
			w := waitingDelete(t, action)
			w.fake(func(f *managedFakePVE) { f.exists = false })
			effects := providerEffectsOf(w.pve)
			a := passUntil(t, w.ts, w.actor.UID, 6, tc.done)
			if a.Unsent() != "" {
				t.Fatalf("%s of an absent container kept the unsent mark: %+v", action, a)
			}
			tc.effect(&effects)
			if got := providerEffectsOf(w.pve); got != effects {
				t.Fatalf("%s effects: %+v want %+v", action, got, effects)
			}
		})
	}
}

// Test277WaitingStopRefusalClaimsNoState: Stop записан намерением, но сервер
// остановился до его отправки, и с тех пор проход CT не наблюдал: она могла
// остановиться или исчезнуть мимо brood. Отказ называет ожидание остановки,
// но не утверждает, что CT работает.
func Test277WaitingStopRefusalClaimsNoState(t *testing.T) {
	for name, container := range map[string]func(*managedFakePVE){
		"running": func(*managedFakePVE) {},
		"absent":  func(f *managedFakePVE) { f.exists, f.running = false, false },
	} {
		t.Run(name, func(t *testing.T) {
			w := runningUnsentWorker(t)
			if _, err := w.ts.srv.store.BeginManagedActorOperation(context.Background(), w.actor.UID, w.actor.Revision, store.NewID(), "reset"); err != nil {
				t.Fatal(err)
			}
			w.fake(container)
			waiting := w.get(t)
			effects := providerEffectsOf(w.pve)
			says := `actor "worker" is waiting to stop its container (Stop is not yet requested); reconcile retries the stop on every pass`
			for _, change := range []string{"start", "delete"} {
				ok, text, _ := w.call(t, change, "")
				if reason := refusalText(text); ok || !strings.Contains(reason, change+" refused: "+says) {
					t.Errorf("%s while the stop is not yet requested: %v %s", change, ok, reason)
				}
			}
			got := operatorUpdate(w.ts, w.alice, w.actor.SwarmID)(t, "worker")
			if got.ok || !strings.Contains(got.text, "update refused: "+says) {
				t.Errorf("update while the stop is not yet requested: %+v", got)
			}
			expectUnchanged(t, w.ts, w.pve, waiting, effects)
		})
	}
}
