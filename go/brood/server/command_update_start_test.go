package server

// FEATURE: The start stage of an update is resumed safely (#133).
// SPEC: After the confirmed stop of an update the actor is retained in its
// stopped container until the start is requested from Proxmox. Until then a
// failure before any Proxmox write (a read of ownership, digest or status, an
// unavailable configuration) leaves the start pending and the next reconcile
// pass retries it, rather than declaring the outcome uncertain. The start gets
// its own pass budget, not what the stop wait left. A server that stopped
// between the stop confirmation and the start request resumes the start. Once
// the start is requested, a lost reply or an unrecorded task stays uncertain
// and the container is never started twice. A bootstrap write is not a start:
// an ambiguous one (a lost reply, a 5xx, the server stopping during the write)
// leaves the start pending, and the next pass checks the stopped container
// again and writes the same bootstrap under the digest CAS. A confirmed
// bootstrap write is not repeated while the configuration digest is unchanged.
// IMPLEMENTATION: reconcilePass over the managed fake; the fake's read
// failures, stop wait and accepted-task hooks inject each fault.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// passActor проходит reconcile одного актора, как reconcileOnce: со своим
// сроком и с записью ошибки прохода. Фейк PVE держит одну CT, поэтому
// соседние акторы не участвуют.
func passActor(t *testing.T, ts *substrateServer, uid string) store.ManagedActor {
	t.Helper()
	return passActorUnder(t, context.Background(), ts, uid)
}

// passActorUnder — passActor под контекстом сервера parent: его отмена —
// остановка сервера посреди прохода.
func passActorUnder(t *testing.T, parent context.Context, ts *substrateServer, uid string) store.ManagedActor {
	t.Helper()
	ctx := context.Background()
	a, err := ts.srv.store.GetManagedActor(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	m := ts.srv.managed
	m.lifecycle.Lock()
	m.reconcilePass(parent, a)
	m.lifecycle.Unlock()
	a, err = ts.srv.store.GetManagedActor(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// passUntil проходит reconcile, пока done не станет истиной; не больше limit
// проходов.
func passUntil(t *testing.T, ts *substrateServer, uid string, limit int, done func(store.ManagedActor) bool) store.ManagedActor {
	t.Helper()
	var a store.ManagedActor
	for range limit {
		if a = passActor(t, ts, uid); done(a) {
			return a
		}
	}
	t.Fatalf("actor did not settle in %d passes: %+v", limit, a)
	return a
}

// acceptedUpdate — update работающего worker с набора oldRelease принят.
func acceptedUpdate(t *testing.T) (*substrateServer, store.ManagedActor, *managedFakePVE, providerEffects) {
	t.Helper()
	ts, alice, master, worker, pve, _ := updateSetup(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	effects := providerEffectsOf(pve)
	if got := operatorUpdate(ts, alice, master.SwarmID)(t, "worker"); !got.ok || got.outcome != "accepted" {
		t.Fatalf("update: %+v", got)
	}
	effects.stops++
	return ts, worker, pve, effects
}

// execDB пишет в базу отдельным соединением: то, что сделал бы другой
// процесс, без побочных эффектов сервера.
func execDB(t *testing.T, ts *substrateServer, statement string, args ...any) {
	db, err := sql.Open("sqlite", ts.dbPath)
	if err != nil {
		t.Errorf("open: %v", err)
		return
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=10000`); err != nil {
		t.Errorf("busy timeout: %v", err)
	}
	if _, err := db.Exec(statement, args...); err != nil {
		t.Errorf("%s: %v", statement, err)
	}
}

func startPending(a store.ManagedActor) bool {
	return a.Operation == "start" && a.Stage == "start" && a.UPID == ""
}

const uncertainOutcome = "Proxmox operation outcome is uncertain"

func TestUpdateRetriesStartAfterReadFailure(t *testing.T) {
	for name, fail := range map[string]func(*managedFakePVE){
		"bootstrap read": func(pve *managedFakePVE) { pve.bootstrapReadFailures = 1 },
		"start read":     func(pve *managedFakePVE) { pve.startReadFailures = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			ts, worker, pve, want := acceptedUpdate(t)
			pve.mu.Lock()
			fail(pve)
			pve.mu.Unlock()
			// Отказ чтения до записи оставляет start ждать, не неизвестным.
			failed := passUntil(t, ts, worker.UID, 4, func(a store.ManagedActor) bool { return a.Error != "" })
			if !startPending(failed) || strings.Contains(failed.Error, uncertainOutcome) {
				t.Fatalf("read failure before any write: %+v", failed)
			}
			restarted := passUntil(t, ts, worker.UID, 6, actorRunning)
			if restarted.OperationID != failed.OperationID || restarted.Incarnation != worker.Incarnation {
				t.Fatalf("retried start replaced the intent: %+v", restarted)
			}
			// Bootstrap пишется один раз: перед вторым Start после отказа
			// чтения Start digest конфигурации тот же, что после записи.
			want.starts++
			want.bootstraps++
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("provider effects: %+v want %+v", got, want)
			}
		})
	}
}

func TestUpdateStartHasItsOwnBudget(t *testing.T) {
	ts, worker, pve, want := acceptedUpdate(t)
	const budget = 2 * time.Second
	ts.srv.managed.passTimeout = budget
	// Ожидание stop-задачи съело три четверти срока прохода.
	pve.mu.Lock()
	pve.stopWait = budget * 3 / 4
	pve.bootstrapBudgets = nil
	pve.mu.Unlock()
	passUntil(t, ts, worker.UID, 6, actorRunning)
	pve.mu.Lock()
	budgets := append([]time.Duration(nil), pve.bootstrapBudgets...)
	pve.mu.Unlock()
	if len(budgets) != 1 || budgets[0] < budget/2 {
		t.Fatalf("start stage budgets %v of %v: the start ran on what the stop wait left", budgets, budget)
	}
	want.starts++
	want.bootstraps++
	if got := providerEffectsOf(pve); got != want {
		t.Fatalf("provider effects: %+v want %+v", got, want)
	}
}

func TestUpdateResumesStartAfterServerStop(t *testing.T) {
	ts, worker, pve, want := acceptedUpdate(t)
	ctx := context.Background()
	intent, err := ts.srv.store.GetManagedActor(ctx, worker.UID)
	if err != nil {
		t.Fatal(err)
	}
	// Прежний сервер дождался stop-задачи, зафиксировал остановку и
	// остановился, не запросив Start.
	if _, err := ts.srv.store.ConfirmManagedActorStage(ctx, intent.UID, intent.Revision, intent.OperationID, "stop", "stopped"); err != nil {
		t.Fatal(err)
	}
	restarted := passUntil(t, ts, worker.UID, 6, actorRunning)
	if restarted.OperationID != intent.OperationID || restarted.Incarnation != worker.Incarnation || restarted.CTID != worker.CTID {
		t.Fatalf("resumed start replaced the intent: %+v", restarted)
	}
	want.starts++
	want.bootstraps++
	if got := providerEffectsOf(pve); got != want {
		t.Fatalf("provider effects: %+v want %+v", got, want)
	}
}

func TestUpdateNeverStartsTwice(t *testing.T) {
	for name, tc := range map[string]struct {
		fault      func(*testing.T, *substrateServer, *managedFakePVE, string)
		starts     int
		bootstraps int
	}{
		"start reply lost": {fault: func(_ *testing.T, _ *substrateServer, pve *managedFakePVE, _ string) { pve.uncertainStart = true }, starts: 1, bootstraps: 1},
		// Ответ Start потерян, а задача в очереди или упала: CT
		// остановлена, как до запроса, и всё же Start второй раз не идёт.
		"start reply lost, container still stopped": {fault: func(_ *testing.T, _ *substrateServer, pve *managedFakePVE, _ string) {
			pve.uncertainStart = true
			pve.afterStart = func() {
				pve.mu.Lock()
				pve.running = false
				pve.mu.Unlock()
			}
		}, starts: 1, bootstraps: 1},
		// PVE принял Start, а его UPID не записан: так выглядит и сервер,
		// остановившийся между отправкой Start и записью UPID. Задача ещё в
		// очереди, CT пока остановлена: вид CT не доказывает, что Start не
		// отправлялся.
		"start task unrecorded": {fault: func(t *testing.T, ts *substrateServer, pve *managedFakePVE, uid string) {
			pve.afterStart = func() {
				pve.mu.Lock()
				pve.running = false
				pve.mu.Unlock()
				execDB(t, ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, uid)
			}
		}, starts: 1, bootstraps: 1},
	} {
		t.Run(name, func(t *testing.T) {
			ts, worker, pve, want := acceptedUpdate(t)
			pve.mu.Lock()
			tc.fault(t, ts, pve, worker.UID)
			pve.mu.Unlock()
			var a store.ManagedActor
			for range 8 {
				a = passActor(t, ts, worker.UID)
			}
			if !startPending(a) || a.Phase != "error" || a.Error != managedProviderError(proxmox.ErrUncertain) {
				t.Fatalf("unknown start outcome: %+v", a)
			}
			want.starts += tc.starts
			want.bootstraps += tc.bootstraps
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("provider effects: %+v want %+v", got, want)
			}
		})
	}
}

// confirmedStop — update принят, и проход подтвердил его остановку: start
// ждёт запроса.
func confirmedStop(t *testing.T) (*substrateServer, store.ManagedActor, *managedFakePVE, providerEffects) {
	t.Helper()
	ts, worker, pve, want := acceptedUpdate(t)
	if a := passActor(t, ts, worker.UID); !startPending(a) || a.Phase != "stopped" || a.Error != "" {
		t.Fatalf("stop not confirmed: %+v", a)
	}
	return ts, worker, pve, want
}

// TestUpdateRunningContainerBeforeStartIsUnknown: Start не запрошен, а CT
// работает — её запустили мимо brood. Это не подтверждённый start, а
// неизвестный исход: Start не идёт, актор не объявлен работающим.
func TestUpdateRunningContainerBeforeStartIsUnknown(t *testing.T) {
	ts, worker, pve, want := confirmedStop(t)
	pve.mu.Lock()
	pve.running = true
	pve.mu.Unlock()
	var a store.ManagedActor
	for range 4 {
		a = passActor(t, ts, worker.UID)
	}
	if !startPending(a) || a.Phase != "error" || a.Error != managedProviderError(proxmox.ErrUncertain) {
		t.Fatalf("running container before Start: %+v", a)
	}
	if got := providerEffectsOf(pve); got != want {
		t.Fatalf("provider effects: %+v want %+v", got, want)
	}
}

// TestUpdateStartSurvivesCancelAfterMark: сервер останавливается после
// отметки provisioning, пока Start ещё читает владение. Start не отправлен:
// отказ чтения записывается вопреки отмене, start снова ждёт запроса, и
// следующий сервер запускает CT один раз.
func TestUpdateStartSurvivesCancelAfterMark(t *testing.T) {
	ts, worker, pve, want := confirmedStop(t)
	server, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	pve.mu.Lock()
	pve.beforeStart = shutdown
	pve.mu.Unlock()
	stopped := passActorUnder(t, server, ts, worker.UID)
	if !startPending(stopped) || stopped.Phase != "stopped" || strings.Contains(stopped.Error, uncertainOutcome) {
		t.Fatalf("start canceled before any request: %+v", stopped)
	}
	pve.mu.Lock()
	pve.beforeStart = nil
	pve.mu.Unlock()
	restarted := passUntil(t, ts, worker.UID, 4, actorRunning)
	if restarted.OperationID != stopped.OperationID {
		t.Fatalf("resumed start replaced the intent: %+v", restarted)
	}
	want.starts++
	want.bootstraps++
	if got := providerEffectsOf(pve); got != want {
		t.Fatalf("provider effects: %+v want %+v", got, want)
	}
}

// TestUpdateRefusedStartWritesBootstrapOnce: Start отказывает явно (403
// после отзыва VM.PowerMgmt), и каждый проход повторяет его, но bootstrap env
// пишется один раз: digest конфигурации тот же, что после подтверждённой
// записи. Правка конфигурации мимо brood меняет digest, и следующий повтор
// пишет env снова.
func TestUpdateRefusedStartWritesBootstrapOnce(t *testing.T) {
	ts, worker, pve, want := confirmedStop(t)
	pve.mu.Lock()
	pve.startError = proxmox.ErrPermission
	pve.mu.Unlock()
	var a store.ManagedActor
	for range 6 {
		a = passActor(t, ts, worker.UID)
	}
	if !startPending(a) || a.Phase != "stopped" || a.Error != managedProviderError(proxmox.ErrPermission) {
		t.Fatalf("refused Start: %+v", a)
	}
	want.bootstraps++
	if got := providerEffectsOf(pve); got != want {
		t.Fatalf("refused Start rewrote the bootstrap: %+v want %+v", got, want)
	}
	pve.mu.Lock()
	pve.digest++
	pve.mu.Unlock()
	passActor(t, ts, worker.UID)
	want.bootstraps++
	if got := providerEffectsOf(pve); got != want {
		t.Fatalf("changed configuration kept the bootstrap: %+v want %+v", got, want)
	}
	pve.mu.Lock()
	pve.startError = nil
	pve.mu.Unlock()
	passUntil(t, ts, worker.UID, 4, actorRunning)
	want.starts++
	if got := providerEffectsOf(pve); got != want {
		t.Fatalf("provider effects: %+v want %+v", got, want)
	}
}

// TestUpdateRetriesUnconfirmedBootstrap: запись bootstrap env, чей исход
// неизвестен, — не Start. CT остаётся остановленной, start ждёт, и следующий
// проход снова проверяет CT и пишет то же содержимое через digest CAS, а
// потом запускает её один раз. Остановка сервера посреди записи не оставляет
// диагностики: следующий сервер продолжает тот же start.
func TestUpdateRetriesUnconfirmedBootstrap(t *testing.T) {
	for name, tc := range map[string]struct {
		fault  func(pve *managedFakePVE, shutdown func())
		detail string
	}{
		"5xx or lost reply": {fault: func(pve *managedFakePVE, _ func()) { pve.bootstrapUncertain = 1 }, detail: managedProviderError(errBootstrapUnconfirmed)},
		"server stops during the write": {fault: func(pve *managedFakePVE, shutdown func()) {
			pve.duringBootstrap = func() {
				shutdown()
				pve.mu.Lock()
				pve.duringBootstrap = nil
				pve.mu.Unlock()
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ts, worker, pve, want := confirmedStop(t)
			server, shutdown := context.WithCancel(context.Background())
			defer shutdown()
			pve.mu.Lock()
			tc.fault(pve, shutdown)
			pve.mu.Unlock()
			waiting := passActorUnder(t, server, ts, worker.UID)
			if !startPending(waiting) || waiting.Phase != "stopped" || waiting.Error != tc.detail {
				t.Fatalf("unconfirmed bootstrap write: %+v", waiting)
			}
			want.bootstraps++
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("unconfirmed bootstrap effects: %+v want %+v", got, want)
			}
			restarted := passUntil(t, ts, worker.UID, 4, actorRunning)
			if restarted.OperationID != waiting.OperationID || restarted.Incarnation != worker.Incarnation {
				t.Fatalf("retried bootstrap replaced the intent: %+v", restarted)
			}
			want.bootstraps++
			want.starts++
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("provider effects: %+v want %+v", got, want)
			}
		})
	}
}
