package server

// FEATURE: Приватные automatic begin и fresh stage submission (#292).
// SPEC: Exact provenance/revision, FULL commit до эффекта, lifecycle и оба owner
// gate через provider/bookkeeping; observation и потерянный ответ не дают replay.
// IMPLEMENTATION: Приватные schema54 adapters с local callbacks и FULL stage reserve.
// RESULT: IMPLEMENTED — focused/race и существующие create/persistent child проверки прошли.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

type lifecyclePVE struct {
	*managedFakePVE
	calls      atomic.Int32
	before     func(context.Context, string, proxmox.OwnedCT) error
	after      func()
	replyError error
	lost       bool
}

func (p *lifecyclePVE) issue(ctx context.Context, stage string, ref proxmox.OwnedCT) (string, error) {
	p.calls.Add(1)
	if p.before != nil {
		if err := p.before(ctx, stage, ref); err != nil {
			return "", err
		}
	}
	var id string
	var err error
	switch stage {
	case "start":
		id, err = p.managedFakePVE.Start(ctx, ref)
	case "stop":
		id, err = p.managedFakePVE.Stop(ctx, ref)
	case "delete":
		id, err = p.managedFakePVE.Delete(ctx, ref)
	}
	if p.after != nil {
		p.after()
	}
	if p.lost {
		return "", proxmox.ErrUncertain
	}
	if p.replyError != nil {
		return id, p.replyError
	}
	return id, err
}
func (p *lifecyclePVE) Start(c context.Context, r proxmox.OwnedCT) (string, error) {
	return p.issue(c, "start", r)
}
func (p *lifecyclePVE) Stop(c context.Context, r proxmox.OwnedCT) (string, error) {
	return p.issue(c, "stop", r)
}
func (p *lifecyclePVE) Delete(c context.Context, r proxmox.OwnedCT) (string, error) {
	return p.issue(c, "delete", r)
}

func lifecycleServerFixture(t *testing.T, stage string) (*dispatchServer, store.EphemeralChildIntent, store.ManagedActor, *lifecyclePVE) {
	t.Helper()
	ctx := context.Background()
	var d *dispatchServer
	var i store.EphemeralChildIntent
	var a store.ManagedActor
	var f *managedFakePVE
	if stage == "start" {
		var p *ephemeralCreatePVE
		d, i, a, p = ephemeralCreateFixture(t)
		var err error
		a, err = d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, i.Parent, i.ID)
		if err != nil {
			t.Fatal(err)
		}
		a, err = d.srv.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "create", "stopped")
		if err != nil {
			t.Fatal(err)
		}
		f = p.managedFakePVE
	} else {
		var child store.SwarmChildTask
		d, a, child = runningEphemeralChildReportFixture(t)
		var err error
		i, err = d.srv.store.GetEphemeralChildIntentForTask(ctx, child.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = d.srv.store.CompleteSwarmChild(ctx, i.Parent, child.RequestID, child.Executor, child.Revision, "success", "Independent retained result", d.srv.managed.childAuthority)
		if err != nil {
			t.Fatal(err)
		}
		second, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "other"))
		if err != nil {
			t.Fatal("second child:", err)
		}
		_, err = d.srv.store.CancelSwarmChild(ctx, i.Parent, second.RequestID, second.Revision, d.srv.managed.childAuthority)
		if err != nil {
			t.Fatal("cancel second:", err)
		}
		_, err = d.srv.store.JoinSwarmChildren(ctx, i.Parent, d.srv.managed.childAuthority)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.srv.store.SealEphemeralChildResult(ctx, i.ID, d.srv.managed.childAuthority); err != nil {
			t.Fatal(err)
		}
		f = &managedFakePVE{server: d.srv, exists: true, running: true, spec: d.srv.managed.createSpec(a)}
	}
	pool, err := d.srv.store.GetManagedPool(ctx, a.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	r := d.srv.managed.registry.pools[pool.Connection].runtime
	r.actors = pool.MaxActors
	r.cpuMillicores = pool.Resources.CPUMillicores
	r.memoryBytes = pool.Resources.MemoryBytes
	r.diskGiB = pool.Resources.DiskBytes / gib
	p := &lifecyclePVE{managedFakePVE: f}
	d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return p, nil }
	delete(d.srv.managed.clients, a.PoolUID)
	kind := "start"
	if stage != "start" {
		kind = "cleanup"
	}
	createSQL(t, d, `CREATE TRIGGER lifecycle_begin_full BEFORE INSERT ON ephemeral_child_lifecycles WHEN (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'lifecycle begin writer requires FULL'); END`)
	a, err = d.srv.beginEphemeralChildLifecycle(ctx, d.master.DelegatorID, i.Parent, i.ID, kind)
	if err != nil {
		t.Fatal(err)
	}
	if stage == "delete" {
		a, err = d.srv.submitEphemeralChildLifecycleStage(ctx, d.master.DelegatorID, i.Parent, a.UID, a.Incarnation, a.OperationID, "stop", a.Revision)
		if err != nil {
			t.Fatal(err)
		}
		a, err = d.srv.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "stop", "stopped")
		if err != nil {
			t.Fatal(err)
		}
		p.calls.Store(0)
	}
	return d, i, a, p
}
func lifecycleSubmit(d *dispatchServer, i store.EphemeralChildIntent, a store.ManagedActor) (store.ManagedActor, error) {
	return d.srv.submitEphemeralChildLifecycleStage(context.Background(), d.master.DelegatorID, i.Parent, a.UID, a.Incarnation, a.OperationID, a.Stage, a.Revision)
}

func TestEphemeralLifecycleAdapterFreshDurableSlots(t *testing.T) {
	for _, stage := range []string{"start", "stop", "delete"} {
		t.Run(stage, func(t *testing.T) {
			d, i, a, p := lifecycleServerFixture(t, stage)
			createSQL(t, d, `CREATE TRIGGER lifecycle_full BEFORE UPDATE OF phase ON managed_actors WHEN NEW.uid='`+a.UID+`' AND NEW.phase IN ('provisioning','deleting') AND NEW.error='' AND NEW.upid='' AND (OLD.phase<>NEW.phase OR OLD.error<>NEW.error) AND (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'lifecycle writer requires FULL'); END`)
			p.before = func(ctx context.Context, s string, r proxmox.OwnedCT) error {
				if s != stage || r != managedOwned(a) {
					t.Errorf("wrong physical slot/identity: %s %+v", s, r)
				}
				db, err := sql.Open("sqlite", d.dbPath)
				if err != nil {
					return err
				}
				defer db.Close()
				var phase, slot, upid, mark string
				if err = db.QueryRow(`SELECT phase,stage,upid,error FROM managed_actors WHERE uid=?`, a.UID).Scan(&phase, &slot, &upid, &mark); err != nil {
					return err
				}
				want := "deleting"
				if stage == "start" {
					want = "provisioning"
				}
				if phase != want || slot != stage || upid != "" || mark != "" {
					t.Errorf("effect before committed requested marker: %s %s %s %s", phase, slot, upid, mark)
				}
				_, err = db.Exec(`UPDATE users SET username=username WHERE id=?`, d.master.DelegatorID)
				return err
			}
			got, err := lifecycleSubmit(d, i, a)
			if err != nil || got.UPID != stage+"-task" || got.OperationID != a.OperationID || got.Stage != stage {
				t.Fatalf("submission: %+v %v", got, err)
			}
			if stage == "start" {
				if p.bootstraps != 1 {
					t.Fatal("bootstrap not configured")
				}
				for k, v := range p.spec.Env {
					if strings.Contains(strings.ToLower(k), "task") || strings.Contains(v, i.Payload) {
						t.Fatal("task leaked into bootstrap")
					}
				}
			}
			for range 2 {
				_, _ = lifecycleSubmit(d, i, got)
			}
			if p.calls.Load() != 1 {
				t.Fatal("stage repeated")
			}
			current, err := d.srv.store.GetManagedActor(context.Background(), a.UID)
			if err != nil || current.CTID != a.CTID || current.OwnerNonce != a.OwnerNonce || current.Incarnation != a.Incarnation || current.Phase == "deleted" {
				t.Fatal("allocation released or replaced")
			}
		})
	}
}
func TestEphemeralLifecycleAdapterConcurrentAndUnknown(t *testing.T) {
	for _, stage := range []string{"start", "stop", "delete"} {
		for _, scenario := range []string{"concurrent", "lost-reopen", "known-upid-error", "bookkeeping"} {
			t.Run(stage+"/"+scenario, func(t *testing.T) {
				d, i, a, p := lifecycleServerFixture(t, stage)
				switch scenario {
				case "lost-reopen":
					p.lost = true
				case "known-upid-error":
					p.replyError = proxmox.ErrUncertain
				case "bookkeeping":
					p.after = func() {
						createSQL(t, d, `CREATE TRIGGER refuse_lifecycle_task BEFORE UPDATE OF upid ON managed_actors BEGIN SELECT RAISE(ABORT,'bookkeeping failure'); END`)
					}
				}
				if scenario == "concurrent" {
					var wg sync.WaitGroup
					for range 4 {
						wg.Add(1)
						go func() { defer wg.Done(); _, _ = lifecycleSubmit(d, i, a) }()
					}
					wg.Wait()
				} else {
					_, err := lifecycleSubmit(d, i, a)
					if scenario == "bookkeeping" && err == nil {
						t.Fatal("bookkeeping error hidden")
					}
				}
				current, err := d.srv.store.GetManagedActor(context.Background(), a.UID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "known-upid-error" && (current.UPID != stage+"-task" || current.Phase != "error") {
					t.Fatal("known task evidence lost")
				}
				if scenario == "lost-reopen" || scenario == "bookkeeping" {
					reopened, e := store.Open(context.Background(), d.dbPath)
					if e != nil {
						t.Fatal(e)
					}
					defer reopened.Close()
					d.srv.actorDelivery.close()
					old := d.srv.store
					d.srv.store = reopened
					defer func() { d.srv.store = old }()
					p.mu.Lock()
					p.exists = stage != "delete"
					p.running = stage == "stop"
					p.mu.Unlock()
				}
				for range 2 {
					_, _ = lifecycleSubmit(d, i, current)
				}
				if p.calls.Load() != 1 {
					t.Fatalf("physical stage repeated: %d", p.calls.Load())
				}
			})
		}
	}
}
func TestEphemeralLifecycleAdapterRefusesFreshPermissionGuesses(t *testing.T) {
	for _, scenario := range []string{"revision", "operation", "incarnation", "stage", "legacy", "manual", "wrong-state", "absent", "bootstrap-unknown", "owner", "approver", "origin", "parent"} {
		t.Run(scenario, func(t *testing.T) {
			d, i, a, p := lifecycleServerFixture(t, "start")
			switch scenario {
			case "revision":
				a.Revision--
			case "operation":
				a.OperationID = "wrong"
			case "incarnation":
				a.Incarnation++
			case "stage":
				a.Stage = "delete"
			case "legacy":
				createSQL(t, d, `DROP TRIGGER ephemeral_child_lifecycle_delete_guard`)
				createSQL(t, d, `DELETE FROM ephemeral_child_lifecycles`)
			case "manual":
				var err error
				a, err = d.srv.store.BindManagedActorTask(context.Background(), a.UID, a.Revision, a.OperationID, "start", "manual-fixture-start")
				if err != nil {
					t.Fatal(err)
				}
				a, err = d.srv.store.ConfirmManagedActorStage(context.Background(), a.UID, a.Revision, a.OperationID, "start", "running")
				if err != nil {
					t.Fatal(err)
				}
				a, err = d.srv.store.BeginManagedActorOperation(context.Background(), a.UID, a.Revision, store.NewID(), "start")
				if err != nil {
					t.Fatal(err)
				}
			case "wrong-state":
				p.running = true
			case "absent":
				p.exists = false
			case "bootstrap-unknown":
				p.bootstrapUncertain = 1
			case "owner":
				createSQL(t, d, `UPDATE users SET disabled=1 WHERE id=?`, d.master.DelegatorID)
			case "approver":
				createSQL(t, d, `UPDATE users SET is_admin=0 WHERE id=?`, d.master.DelegatorID)
			case "origin":
				if err := d.srv.store.SetProjectRepository(context.Background(), i.Parent.Principal.ProjectID, "https://foreign.invalid/repo"); err != nil {
					t.Fatal(err)
				}
			case "parent":
				i.Parent.RequestID = "wrong"
			}
			_, err := lifecycleSubmit(d, i, a)
			if err == nil && scenario != "bootstrap-unknown" {
				t.Fatal("invalid fresh permission accepted")
			}
			if p.calls.Load() != 0 {
				t.Fatal("unauthorized stage submitted")
			}
			if scenario == "bootstrap-unknown" {
				current, e := d.srv.store.GetManagedActor(context.Background(), a.UID)
				if e != nil || current.Phase != "stopped" || current.UPID != "" {
					t.Fatal("bootstrap ambiguity became issued Start")
				}
				_, e = lifecycleSubmit(d, i, current)
				if e != nil || p.calls.Load() != 1 {
					t.Fatalf("bootstrap not safely retryable: %v", e)
				}
			}
		})
	}
}
func lifecycleOwnerIntervention(d *dispatchServer, a store.ManagedActor) error {
	ctx := context.Background()
	release, err := d.srv.managed.effects.acquire(ctx, a.UID)
	if err != nil {
		return err
	}
	defer release()
	op, err := d.srv.store.AcquireOwnerActorOperation(ctx, a, store.NewID(), "lifecycle-owner", "delete", d.master.DelegatorID, func(ctx context.Context) error {
		return d.srv.authorizeSwarmOwnerIntent(ctx, d.master.DelegatorID, d.swarm)
	})
	if err != nil {
		return err
	}
	return d.srv.store.FinishActorOperation(ctx, op, true)
}

func TestEphemeralLifecycleAdapterGatesAndLifecycle(t *testing.T) {
	for _, stage := range []string{"start", "stop", "delete"} {
		for _, target := range []string{"parent", "executor"} {
			t.Run(stage+"/"+target, func(t *testing.T) {
				d, i, a, p := lifecycleServerFixture(t, stage)
				actor := a
				if target == "parent" {
					actor = d.master
				}
				entered, resume := make(chan struct{}), make(chan struct{})
				defer func() {
					select {
					case <-resume:
					default:
						close(resume)
					}
				}()
				p.before = func(ctx context.Context, _ string, _ proxmox.OwnedCT) error {
					close(entered)
					select {
					case <-resume:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				done := make(chan error, 1)
				go func() { _, e := lifecycleSubmit(d, i, a); done <- e }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("provider not reached")
				}
				if d.srv.managed.lifecycle.TryLock() {
					d.srv.managed.lifecycle.Unlock()
					t.Fatal("lifecycle released before effect")
				}
				ownerDone := make(chan error, 1)
				go func() { ownerDone <- lifecycleOwnerIntervention(d, actor) }()
				deadline := time.Now().Add(5 * time.Second)
				for {
					g := &d.srv.managed.effects
					g.mu.Lock()
					entry := g.entries[actor.UID]
					blocked := entry != nil && entry.users >= 2
					g.mu.Unlock()
					if blocked {
						break
					}
					select {
					case e := <-ownerDone:
						t.Fatalf("owner overtook provider: %v", e)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatal("owner did not reach gate")
					}
					runtime.Gosched()
				}
				close(resume)
				if e := <-done; e != nil {
					t.Fatal(e)
				}
				if e := <-ownerDone; e != nil {
					t.Fatal(e)
				}
				if !d.srv.managed.lifecycle.TryLock() {
					t.Fatal("lifecycle leaked")
				}
				d.srv.managed.lifecycle.Unlock()
			})
		}
	}
}
func TestEphemeralLifecycleAdapterPreflightAndCancel(t *testing.T) {
	for _, target := range []string{"parent", "executor", "cancel"} {
		t.Run(target, func(t *testing.T) {
			d, i, a, p := lifecycleServerFixture(t, "start")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			delete(d.srv.managed.clients, a.PoolUID)
			d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
				if target == "cancel" {
					cancel()
				} else {
					actor := a
					if target == "parent" {
						actor = d.master
					}
					if err := lifecycleOwnerIntervention(d, actor); err != nil {
						t.Fatal(err)
					}
				}
				return p, nil
			}
			_, err := d.srv.submitEphemeralChildLifecycleStage(ctx, d.master.DelegatorID, i.Parent, a.UID, a.Incarnation, a.OperationID, a.Stage, a.Revision)
			if err == nil || p.calls.Load() != 0 {
				t.Fatal("preflight change/cancel ignored")
			}
			if target == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			release, e := d.srv.managed.effects.acquire(context.Background(), i.Parent.Principal.ActorUID, a.UID)
			if e != nil {
				t.Fatal(e)
			}
			release()
			if !d.srv.managed.lifecycle.TryLock() {
				t.Fatal("lifecycle leaked")
			}
			d.srv.managed.lifecycle.Unlock()
		})
	}
}
