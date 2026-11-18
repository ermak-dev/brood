package server

import (
	"context"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
)

type observerFailingReadPVE struct {
	proxmoxAPI
	stage         string
	beforeFailure func(context.Context) error
}

func (p *observerFailingReadPVE) WaitTask(ctx context.Context, upid string) error {
	if p.stage == "wait" {
		return p.beforeFailure(ctx)
	}
	return p.proxmoxAPI.WaitTask(ctx, upid)
}

func (p *observerFailingReadPVE) Status(ctx context.Context, id int) (proxmox.CTStatus, error) {
	if p.stage == "status" {
		return proxmox.CTStatus{}, p.beforeFailure(ctx)
	}
	return p.proxmoxAPI.Status(ctx, id)
}

// BUG: Ошибка provider read обходила повторную authority-проверку diagnostics.
// HYPOTHESIS: Generic error handler писал phase:error после отзыва делегации.
// SOLUTION: Automatic diagnostics повторяют namespace/local scope и CAS перед записью.
// RESULT: FIXED — wait/status RED→GREEN, actor неизменен при отзыве, cache очищен.
// STATUS: FIXED
func TestEphemeralAutomaticObserverRechecksAuthorityOnFailedRead(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"wait", "status"} {
		t.Run(stage, func(t *testing.T) {
			d, intent, actor, pve := lifecycleServerFixture(t, "start")
			requested, err := lifecycleSubmit(d, intent, actor)
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			provider := &observerFailingReadPVE{proxmoxAPI: pve, stage: stage, beforeFailure: func(ctx context.Context) error {
				reads++
				if err := d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true); err != nil {
					t.Fatal(err)
				}
				return proxmox.ErrPermission
			}}
			d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return provider, nil }
			delete(d.srv.managed.clients, actor.PoolUID)
			d.srv.managed.verified.Store(actor.UID, actor.Incarnation)
			d.srv.managed.lifecycle.Lock()
			d.srv.managed.reconcilePass(ctx, requested)
			d.srv.managed.lifecycle.Unlock()
			current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || current != requested || reads != 1 || pve.calls.Load() != 1 {
				t.Fatalf("failed provider read wrote diagnostics after authority revocation: %+v reads=%d effects=%d %v", current, reads, pve.calls.Load(), err)
			}
			if _, verified := d.srv.managed.verified.Load(actor.UID); verified {
				t.Fatal("revoked authority retained current readiness verification")
			}
		})
	}
}

// BUG: Generic reconcile отправлял automatic Start/Stop/Delete без fresh FULL grant.
// HYPOTHESIS: После schema54 provenance runtime всё ещё использовал manual effect path.
// SOLUTION: Положительный manual origin отделён от automatic observation-only пути.
// RESULT: FIXED — unrequested stages, Stop/Delete boundary и registered readiness
// проверены с настоящими Store/PVE adapters; полный Go/Python и targeted race проходят.
// STATUS: FIXED
func TestEphemeralAutomaticObserverRetainsRegisteredReadyExecutor(t *testing.T) {
	ctx := context.Background()
	d, intent, actor, pve := lifecycleServerFixture(t, "start")
	d.onConnected(func(base commandSessionHub, uid string, incarnation int64) bool {
		return uid == actor.UID && incarnation == actor.Incarnation || base.Connected(uid, incarnation)
	})
	requested, err := lifecycleSubmit(d, intent, actor)
	if err != nil {
		t.Fatal(err)
	}
	d.srv.managed.lifecycle.Lock()
	d.srv.managed.reconcilePass(ctx, requested)
	d.srv.managed.lifecycle.Unlock()
	ready, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || ready.Stage != "done" || ready.Phase != "running" {
		t.Fatalf("submitted Start did not become ready through observation: %+v %v", ready, err)
	}
	key := workspaceAttestationKey(d.srv.managed.registry.relaySecret)
	d.reply = func(call guestAttest) (relay.SessionResponse, error) {
		return signedAttestation(t, key, call, call.Generation)
	}
	if _, err := d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "observer-source"); err != nil {
		t.Fatal(err)
	}
	child, err := d.srv.registerEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.srv.managed.lifecycle.Lock()
	d.srv.managed.reconcilePass(ctx, ready)
	d.srv.managed.lifecycle.Unlock()
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current != ready || pve.calls.Load() != 1 {
		t.Fatalf("registration invalidated observed readiness or renewed an effect for child %s: %+v effects=%d %v", child.ID, current, pve.calls.Load(), err)
	}
	d.stop()
	d.srv.managed.lifecycle.Lock()
	d.srv.managed.reconcilePass(ctx, current)
	d.srv.managed.lifecycle.Unlock()
	held, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || held != current || pve.calls.Load() != 1 {
		t.Fatalf("lost scheduling authority became a fabricated physical failure: %+v effects=%d %v", held, pve.calls.Load(), err)
	}
	if _, verified := d.srv.managed.verified.Load(actor.UID); verified {
		t.Fatal("held automatic observation retained a current readiness verification")
	}
}

func TestEphemeralAutomaticObserverDoesNotSubmitUnrequestedStage(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"start", "stop", "delete"} {
		t.Run(stage, func(t *testing.T) {
			d, _, actor, pve := lifecycleServerFixture(t, stage)
			d.srv.managed.lifecycle.Lock()
			d.srv.managed.reconcilePass(ctx, actor)
			d.srv.managed.lifecycle.Unlock()
			if pve.calls.Load() != 0 {
				t.Fatalf("generic reconcile submitted automatic %s without a fresh controller grant: %d", stage, pve.calls.Load())
			}
			current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || current != actor {
				t.Fatalf("unrequested automatic stage lost its exact retained marker: %+v %v", current, err)
			}
		})
	}
}

// BUG: Known-unrequested cleanup Stop оставался навсегда, если CT уже stopped.
// HYPOTHESIS: Общий early return не различал запрет эффекта и допустимое наблюдение.
// SOLUTION: Positive unsent Stop наблюдается без отправки; только owned stopped
// закрывает стадию, а read failure сохраняет phase/marker вместе с диагностикой.
// RESULT: FIXED — оба RED→GREEN, полный Go/Python прогон и focused race проходят.
// STATUS: FIXED
func TestEphemeralAutomaticObserverPreservesUnrequestedStopOnReadFailure(t *testing.T) {
	ctx := context.Background()
	d, _, actor, pve := lifecycleServerFixture(t, "stop")
	pve.mu.Lock()
	pve.inspectError = proxmox.ErrPermission
	pve.absenceError = proxmox.ErrPermission
	pve.mu.Unlock()
	d.srv.managed.lifecycle.Lock()
	d.srv.managed.reconcilePass(ctx, actor)
	d.srv.managed.lifecycle.Unlock()
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current.Stage != "stop" || current.Phase != actor.Phase || current.Unsent() != actor.Unsent() || current.Diagnostic() == "" || pve.calls.Load() != 0 {
		t.Fatalf("read failure was not retained with its positively unrequested stop marker: %+v effects=%d %v", current, pve.calls.Load(), err)
	}
}

func TestEphemeralAutomaticObserverClosesOnlyKnownUnrequestedStoppedStage(t *testing.T) {
	ctx := context.Background()
	d, _, actor, pve := lifecycleServerFixture(t, "stop")
	pve.mu.Lock()
	pve.running = false
	pve.mu.Unlock()
	d.srv.managed.lifecycle.Lock()
	d.srv.managed.reconcilePass(ctx, actor)
	d.srv.managed.lifecycle.Unlock()
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current.Stage != "delete" || !deleteNotRequested(current) || current.OperationID != actor.OperationID || pve.calls.Load() != 0 {
		t.Fatalf("owned stopped CT did not close a proven unrequested Stop without effects: %+v effects=%d %v", current, pve.calls.Load(), err)
	}
}

func TestEphemeralAutomaticObserverDoesNotChainDeleteAfterStop(t *testing.T) {
	ctx := context.Background()
	d, intent, actor, pve := lifecycleServerFixture(t, "stop")
	requested, err := lifecycleSubmit(d, intent, actor)
	if err != nil {
		t.Fatal(err)
	}
	d.srv.managed.lifecycle.Lock()
	d.srv.managed.reconcilePass(ctx, requested)
	d.srv.managed.lifecycle.Unlock()
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current.Stage != "delete" || !deleteNotRequested(current) || pve.calls.Load() != 1 {
		t.Fatalf("stop observation issued Delete instead of retaining the next unrequested stage: %+v effects=%d %v", current, pve.calls.Load(), err)
	}
}

func TestEphemeralAutomaticObserverDoesNotReleaseAfterDelete(t *testing.T) {
	ctx := context.Background()
	d, intent, actor, pve := lifecycleServerFixture(t, "delete")
	requested, err := lifecycleSubmit(d, intent, actor)
	if err != nil {
		t.Fatal(err)
	}
	d.srv.managed.lifecycle.Lock()
	d.srv.managed.reconcilePass(ctx, requested)
	d.srv.managed.lifecycle.Unlock()
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current != requested || pve.calls.Load() != 1 {
		t.Fatalf("generic observer released physical capacity instead of retaining dedicated finalization: %+v effects=%d %v", current, pve.calls.Load(), err)
	}
}
