package server

// BUG: Идущая задача PVE показывается пользователю ошибкой (#106). Живой
// vzcreate шёл 34 с, проход reconcile дал актору 20 с, и actor получил
// phase=error «outcome is uncertain», хотя следующий проход дошёл до running.
// HYPOTHESIS: reconcileOnce сохраняет ошибкой любой отказ WaitTask, включая
// истечение срока самого прохода при задаче в состоянии running.
// SOLUTION: Истёкший срок прохода — ожидание, а не исход: стадия, UPID и
// ревизия остаются, следующий проход ждёт ту же задачу. После 15 минут от
// UpdatedAt — санированная диагностика при сохранённом UPID и резерве.
// RESULT: FIXED — create/start/stop/delete ждут без ложной ошибки и без
// повторных записей; настоящие отказы PVE сохраняются как прежде.
// STATUS: FIXED

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// managedPendingTimeout — короткий срок прохода вместо боевых 20 с.
const managedPendingTimeout = 200 * time.Millisecond

const managedPendingDiagnostic = "Proxmox task has not finished; allocation retained"

// managedPendingPVE держит выбранные задачи в состоянии running: WaitTask
// ждёт срока прохода и отвечает, как настоящий клиент при истёкшем ctx.
type managedPendingPVE struct {
	*managedFakePVE
	pending map[string]bool // под managedFakePVE.mu
}

func (f *managedPendingPVE) WaitTask(ctx context.Context, upid string) error {
	f.mu.Lock()
	pending := f.pending[upid]
	if pending {
		f.waited = append(f.waited, upid)
	}
	f.mu.Unlock()
	if !pending {
		return f.managedFakePVE.WaitTask(ctx, upid)
	}
	<-ctx.Done()
	return fmt.Errorf("%w: %w", proxmox.ErrUncertain, ctx.Err())
}

func (f *managedPendingPVE) setPending(upid string, pending bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending[upid] = pending
}

func (f *managedPendingPVE) writes() [5]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return [5]int{f.creates, f.starts, f.stops, f.deletes, f.bootstraps}
}

func (f *managedPendingPVE) waits(upid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, waited := range f.waited {
		if waited == upid {
			n++
		}
	}
	return n
}

func managedPendingSetup(t *testing.T) (*substrateServer, *http.Client, string, *managedPendingPVE, *managedFakeRelay) {
	t.Helper()
	ts, alice, _, id, pve, hub := managedSetup(t)
	fake := &managedPendingPVE{managedFakePVE: pve, pending: map[string]bool{}}
	ts.srv.managed.clients = map[string]proxmoxAPI{}
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return fake, nil }
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	return ts, alice, id, fake, hub
}

func managedPendingCreate(t *testing.T, ts *substrateServer, alice *http.Client, id, name string) {
	t.Helper()
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": name}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
}

// expectManagedPending проводит два прохода, упирающихся в срок, и требует
// нетронутого durable-состояния: та же ревизия, стадия, UPID, без ошибки.
func expectManagedPending(t *testing.T, ts *substrateServer, id string, pve *managedPendingPVE, stage string) store.ManagedActor {
	t.Helper()
	before := mustManagedActor(t, ts, id)
	if before.Stage != stage || before.UPID == "" {
		t.Fatalf("setup did not bind a %s task: %+v", stage, before)
	}
	writes, waits := pve.writes(), pve.waits(before.UPID)
	ts.srv.managed.passTimeout = managedPendingTimeout
	defer func() { ts.srv.managed.passTimeout = 0 }()
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	a := mustManagedActor(t, ts, id)
	if a.Phase != before.Phase || a.Error != "" || a.Revision != before.Revision || a.Stage != stage || a.UPID != before.UPID || a.OperationID != before.OperationID {
		t.Fatalf("pending %s task surfaced or replaced:\nbefore phase=%s rev=%d upid=%s\nafter  phase=%s rev=%d upid=%s error=%q", stage, before.Phase, before.Revision, before.UPID, a.Phase, a.Revision, a.UPID, a.Error)
	}
	if pve.writes() != writes {
		t.Fatalf("pending %s task resubmitted provider writes: %v -> %v", stage, writes, pve.writes())
	}
	if pve.waits(before.UPID) != waits+2 {
		t.Fatalf("pending %s task was not awaited on every pass", stage)
	}
	return a
}

func TestManagedPendingTaskEveryStageKeepsDurableState(t *testing.T) {
	ts, alice, id, pve, _ := managedPendingSetup(t)
	m := ts.srv.managed
	for _, upid := range []string{"create-task", "start-task", "stop-task", "delete-task"} {
		pve.setPending(upid, true)
	}
	path := "/api/swarms/" + id + "/actors/stages"
	managedPendingCreate(t, ts, alice, id, "stages")

	if a := expectManagedPending(t, ts, id, pve, "create"); a.Phase != "provisioning" {
		t.Fatalf("pending create left provisioning: %s", a.Phase)
	}
	pve.setPending("create-task", false)
	m.reconcileOnce(context.Background())

	if a := expectManagedPending(t, ts, id, pve, "start"); a.Phase != "provisioning" {
		t.Fatalf("pending start left provisioning: %s", a.Phase)
	}
	pve.setPending("start-task", false)
	m.reconcileOnce(context.Background())
	if a := mustManagedActor(t, ts, id); a.Phase != "running" || a.Error != "" {
		t.Fatalf("finished start did not reach running: %+v", a)
	}

	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "stop", r, b, 200)
	if a := expectManagedPending(t, ts, id, pve, "stop"); a.Phase != "stopping" {
		t.Fatalf("pending stop left stopping: %s", a.Phase)
	}
	pve.setPending("stop-task", false)
	m.reconcileOnce(context.Background())
	if a := mustManagedActor(t, ts, id); a.Phase != "stopped" || a.Stage != "done" {
		t.Fatalf("finished stop did not settle: %+v", a)
	}

	r, b, _ = ts.do(alice, "DELETE", path, ts.csrf(alice), nil)
	expectStatus(t, "delete", r, b, 202)
	expectManagedPending(t, ts, id, pve, "delete")
	pve.setPending("delete-task", false)
	m.reconcileOnce(context.Background())
	if a := mustManagedActor(t, ts, id); a.Phase != "deleted" {
		t.Fatalf("finished delete did not retire allocation: %+v", a)
	}
	if got := pve.writes(); got != [5]int{1, 1, 1, 1, 1} {
		t.Fatalf("pending tasks duplicated provider writes: creates, starts, stops, deletes, bootstraps = %v", got)
	}
}

func TestManagedPendingTaskOverdueIsDiagnosedThenSettles(t *testing.T) {
	ts, alice, id, pve, _ := managedPendingSetup(t)
	m := ts.srv.managed
	pve.setPending("create-task", true)
	managedPendingCreate(t, ts, alice, id, "slow")
	before := mustManagedActor(t, ts, id)
	since, err := time.Parse(time.RFC3339Nano, before.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	m.passTimeout = managedPendingTimeout
	m.now = func() time.Time { return since.Add(15*time.Minute - time.Second) }
	m.reconcileOnce(context.Background())
	if a := mustManagedActor(t, ts, id); a.Revision != before.Revision || a.Error != "" {
		t.Fatalf("pending task diagnosed before the limit: %+v", a)
	}

	m.now = func() time.Time { return since.Add(15 * time.Minute) }
	m.reconcileOnce(context.Background())
	a := mustManagedActor(t, ts, id)
	if a.Phase != "error" || a.Error != managedPendingDiagnostic {
		t.Fatalf("overdue task not diagnosed: phase=%s error=%q", a.Phase, a.Error)
	}
	if a.Stage != "create" || a.UPID != before.UPID || a.OperationID != before.OperationID || a.OwnerNonce != before.OwnerNonce || a.CTID != before.CTID || a.Incarnation != before.Incarnation {
		t.Fatalf("overdue diagnostic dropped task evidence or ownership: %+v", a)
	}
	pool, _ := ts.srv.store.GetManagedPool(context.Background(), a.PoolUID)
	if usage, _ := m.poolJSON(context.Background(), pool); usage.UsedActors != "1" {
		t.Fatal("overdue task released capacity")
	}
	r, b, _ := ts.do(alice, "GET", "/api/swarms/"+id+"/actors/slow", "", nil)
	expectStatus(t, "read", r, b, 200)
	if b["status"].(map[string]any)["errorMessage"] != managedPendingDiagnostic {
		t.Fatalf("overdue diagnostic not visible: %v", b["status"])
	}

	m.now = func() time.Time { return since.Add(time.Hour) }
	m.reconcileOnce(context.Background())
	if again := mustManagedActor(t, ts, id); again.Revision != a.Revision {
		t.Fatal("recorded overdue diagnostic rewritten on every pass")
	}

	m.passTimeout = 0
	pve.setPending("create-task", false)
	for range 2 {
		m.reconcileOnce(context.Background())
	}
	a = mustManagedActor(t, ts, id)
	if a.Phase != "running" || a.Error != "" || a.Stage != "done" {
		t.Fatalf("finished overdue task did not settle to running: %+v", a)
	}
	if got := pve.writes(); got != [5]int{1, 1, 0, 0, 1} {
		t.Fatalf("overdue task duplicated provider writes: creates, starts, stops, deletes, bootstraps = %v", got)
	}
}

func TestManagedPendingTaskRealFailuresUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, stage, want string
		err               error
	}{
		{"task failed", "done", "Proxmox task failed; allocation retained", proxmox.ErrTaskFailed},
		// Свой таймаут HTTP-клиента — сбой сети, а не истёкший срок прохода.
		{"request timeout", "create", "Proxmox operation outcome is uncertain; recorded CTID and reservation retained", fmt.Errorf("%w: %w", proxmox.ErrUncertain, context.DeadlineExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, alice, id, pve, _ := managedPendingSetup(t)
			m := ts.srv.managed
			pve.setPending("create-task", true)
			managedPendingCreate(t, ts, alice, id, "fails")
			before := mustManagedActor(t, ts, id)
			m.passTimeout = managedPendingTimeout
			m.reconcileOnce(context.Background())
			m.passTimeout = 0
			pve.mu.Lock()
			pve.pending["create-task"] = false
			pve.taskErrors = map[string]error{"create-task": tc.err}
			pve.mu.Unlock()
			m.reconcileOnce(context.Background())
			a := mustManagedActor(t, ts, id)
			if a.Phase != "error" || a.Error != tc.want || a.Stage != tc.stage || a.UPID != before.UPID {
				t.Fatalf("real failure changed: phase=%s stage=%s error=%q upid=%s", a.Phase, a.Stage, a.Error, a.UPID)
			}
			if got := pve.writes(); got != [5]int{1, 0, 0, 0, 0} {
				t.Fatalf("failed task replayed or advanced: %v", got)
			}
		})
	}
}

func TestManagedTaskPendingOnlyOnPassDeadline(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deadline := fmt.Errorf("%w: %w", proxmox.ErrUncertain, context.DeadlineExceeded)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"pass deadline", expired, deadline, true},
		{"request timeout with live pass", context.Background(), deadline, false},
		{"other uncertainty at deadline", expired, fmt.Errorf("%w: invalid task status", proxmox.ErrUncertain), false},
		{"task failed at deadline", expired, proxmox.ErrTaskFailed, false},
		{"canceled pass", canceled, fmt.Errorf("%w: %w", proxmox.ErrUncertain, context.Canceled), false},
		{"permission", expired, proxmox.ErrPermission, false},
		{"plain network", expired, errors.New("connection reset"), false},
	} {
		if got := managedTaskPending(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: pending=%v, want %v", tc.name, got, tc.want)
		}
	}
}
