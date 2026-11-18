package server

// FEATURE: Приватные begin/retry fences и доверенное not-sent evidence (#292).
// SPEC: Begin не создаёт seal и не заменяет manual holds; только явный отказ
// provider до эффекта разрешает вернуть unsent той же automatic operation.
// IMPLEMENTATION: Реальные Store/manual boundaries и SQLite writer blocking.
// RESULT: IMPLEMENTED — focused/race проверяют fences, отсутствие replay и отпускание gates.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

func TestEphemeralLifecycleAdapterBeginRetryAndMissingSeal(t *testing.T) {
	for _, stage := range []string{"start", "stop"} {
		t.Run(stage, func(t *testing.T) {
			d, i, a, p := lifecycleServerFixture(t, stage)
			kind := "start"
			if stage == "stop" {
				kind = "cleanup"
			}
			got, err := d.srv.beginEphemeralChildLifecycle(context.Background(), d.master.DelegatorID, i.Parent, i.ID, kind)
			if err != nil || got != a || p.calls.Load() != 0 {
				t.Fatalf("begin retry replaced operation or submitted: %+v %v", got, err)
			}
			if stage == "stop" {
				v, e := d.srv.store.GetSwarmStep(context.Background(), d.swarm, i.Parent.Sequence)
				if e != nil || v.State != "completed" {
					t.Fatal("cleanup not authorized against completed historical parent")
				}
			}
			if stage == "start" {
				_, err = d.srv.beginEphemeralChildLifecycle(context.Background(), d.master.DelegatorID, i.Parent, i.ID, "cleanup")
				if err == nil {
					t.Fatal("cleanup invented missing seal")
				}
				if _, err = d.srv.store.GetEphemeralChildResultSeal(context.Background(), i.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatal("begin minted seal")
				}
				if _, err = d.srv.beginEphemeralChildLifecycle(context.Background(), d.master.DelegatorID, i.Parent, i.ID, "reset"); err == nil {
					t.Fatal("unknown kind accepted")
				}
			}
			if err = lifecycleOwnerIntervention(d, a); err != nil {
				t.Fatal(err)
			}
			if _, err = d.srv.beginEphemeralChildLifecycle(context.Background(), d.master.DelegatorID, i.Parent, i.ID, kind); err == nil {
				t.Fatal("begin bypassed manual intervention")
			}
			current, err := d.srv.store.GetManagedActor(context.Background(), a.UID)
			if err != nil || current != a || p.calls.Load() != 0 {
				t.Fatal("begin mutated held actor")
			}
		})
	}
}
func TestEphemeralLifecycleAdapterOnlyTrustedNotSentRearms(t *testing.T) {
	for _, stage := range []string{"start", "stop", "delete"} {
		t.Run(stage, func(t *testing.T) {
			d, i, a, p := lifecycleServerFixture(t, stage)
			p.before = func(context.Context, string, proxmox.OwnedCT) error { return proxmox.ErrPermission }
			pending, err := lifecycleSubmit(d, i, a)
			if err != nil || pending.OperationID != a.OperationID || pending.UPID != "" {
				t.Fatalf("known not-sent evidence not retained: %+v %v", pending, err)
			}
			if stage == "start" && !startNotRequested(pending) || stage == "stop" && !stopNotRequested(pending) || stage == "delete" && !deleteNotRequested(pending) {
				t.Fatal("not-sent stage not restored")
			}
			if stage == "start" && p.bootstraps != 1 {
				t.Fatal("bootstrap not installed")
			}
			p.before = nil
			got, err := lifecycleSubmit(d, i, pending)
			if err != nil || got.UPID != stage+"-task" || p.calls.Load() != 2 {
				t.Fatalf("same operation not retryable after trusted evidence: %+v %v", got, err)
			}
			if stage == "start" && p.bootstraps != 1 {
				t.Fatal("unchanged bootstrap was not idempotent")
			}
		})
	}
}
func TestEphemeralLifecycleAdapterCanceledOwnerGateReleasesLifecycle(t *testing.T) {
	d, i, a, p := lifecycleServerFixture(t, "start")
	release, err := d.srv.managed.effects.acquire(context.Background(), i.Parent.Principal.ActorUID, a.UID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := d.srv.submitEphemeralChildLifecycleStage(ctx, d.master.DelegatorID, i.Parent, a.UID, a.Incarnation, a.OperationID, a.Stage, a.Revision)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g := &d.srv.managed.effects
		g.mu.Lock()
		entry := g.entries[i.Parent.Principal.ActorUID]
		waiting := entry != nil && entry.users >= 2
		g.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("submission did not wait for parent gate")
		}
		runtime.Gosched()
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled gate leaked")
	}
	if p.calls.Load() != 0 {
		t.Fatal("canceled waiter submitted")
	}
	if !d.srv.managed.lifecycle.TryLock() {
		t.Fatal("canceled gate leaked lifecycle")
	}
	d.srv.managed.lifecycle.Unlock()
}
func TestEphemeralLifecycleAdapterFinalLocalScopeAfterBootstrap(t *testing.T) {
	for _, change := range []string{"owner", "approver", "origin", "namespace"} {
		t.Run(change, func(t *testing.T) {
			d, i, a, p := lifecycleServerFixture(t, "start")
			p.duringBootstrap = func() {
				switch change {
				case "owner":
					createSQL(t, d, `UPDATE users SET disabled=1 WHERE id=?`, d.master.DelegatorID)
				case "approver":
					createSQL(t, d, `UPDATE users SET is_admin=0 WHERE id=?`, d.master.DelegatorID)
				case "origin":
					if err := d.srv.store.SetProjectRepository(context.Background(), i.Parent.Principal.ProjectID, "https://foreign.invalid/repo"); err != nil {
						t.Error(err)
					}
				case "namespace":
					createSQL(t, d, `UPDATE swarms SET atespace_uid='changed' WHERE id=?`, d.swarm)
				}
			}
			_, err := lifecycleSubmit(d, i, a)
			if err == nil || p.calls.Load() != 0 {
				t.Fatal("final local scope change ignored")
			}
			current, err := d.srv.store.GetManagedActor(context.Background(), a.UID)
			if err != nil || !startNotRequested(current) {
				t.Fatal("failed final check consumed Start")
			}
		})
	}
}
func TestEphemeralLifecycleAdapterBookkeepingRetainsAllLocks(t *testing.T) {
	d, i, a, p := lifecycleServerFixture(t, "start")
	raw, err := sql.Open("sqlite", d.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	accepted, resume := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	p.after = func() {
		if _, e := raw.Exec(`BEGIN IMMEDIATE`); e != nil {
			t.Error(e)
		}
		close(accepted)
	}
	done := make(chan error, 1)
	go func() { _, e := lifecycleSubmit(d, i, a); done <- e }()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("no provider reply")
	}
	// SQLite writer blocks task binding after the provider reply. The submission
	// must still exclude both owner paths and ordinary reconciliation.
	if d.srv.managed.lifecycle.TryLock() {
		d.srv.managed.lifecycle.Unlock()
		t.Fatal("bookkeeping lost lifecycle")
	}
	waiters := make(chan error, 2)
	for _, uid := range []string{i.Parent.Principal.ActorUID, a.UID} {
		go func() {
			release, e := d.srv.managed.effects.acquire(context.Background(), uid)
			if e == nil {
				release()
			}
			waiters <- e
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			g := &d.srv.managed.effects
			g.mu.Lock()
			entry := g.entries[uid]
			waiting := entry != nil && entry.users >= 2
			g.mu.Unlock()
			if waiting {
				break
			}
			select {
			case e := <-waiters:
				t.Fatalf("owner overtook task bookkeeping: %v", e)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("owner did not reach bookkeeping gate")
			}
			runtime.Gosched()
		}
	}
	if _, err = raw.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	close(resume)
	for range 2 {
		select {
		case e := <-waiters:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("owner gate not released after bookkeeping")
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("task binding did not finish")
	}
}
