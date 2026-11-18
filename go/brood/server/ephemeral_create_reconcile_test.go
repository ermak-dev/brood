package server

import (
	"context"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// BUG: Фоновый reconcile теряет UPID конкурентного ephemeral Create (#292).
// HYPOTHESIS: Reconcile записывает диагностику empty-UPID до recordTask и меняет ревизию.
// SOLUTION: Lifecycle удерживается до сохранения ответа провайдера.
// RESULT: FIXED — исходный UPID сохраняется, один Create и ни одного Start/Stop/Delete.
// STATUS: FIXED
func TestEphemeralCreateRetainsTaskDuringBackgroundReconcile(t *testing.T) {
	d, intent, actor, provider := ephemeralCreateFixture(t)
	entered := make(chan struct{})
	resume := make(chan struct{})
	provider.before = func(ctx context.Context, _ proxmox.CTSpec) error {
		close(entered)
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	type result struct {
		actor store.ManagedActor
		err   error
	}
	created := make(chan result, 1)
	go func() {
		a, err := d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
		created <- result{a, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("Create did not reach the provider")
	}
	reconcileCtx, stopReconcile := context.WithCancel(ctx)
	defer stopReconcile()
	reconciled := make(chan struct{})
	go func() {
		defer close(reconciled)
		d.srv.managed.reconcileOnce(reconcileCtx)
	}()

	// Отмена прохода перед ответом Create не позволяет ему затем запустить гостя:
	// измеряется только потеря исходного UPID во время незавершённого Create.
	window := time.NewTimer(time.Second)
	defer window.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var concurrentRevision int64
observe:
	for {
		select {
		case <-tick.C:
			current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err == nil && current.Revision != actor.Revision {
				concurrentRevision = current.Revision
				break observe
			}
		case <-window.C:
			break observe
		case <-ctx.Done():
			break observe
		}
	}
	stopReconcile()
	close(resume)
	var got result
	select {
	case got = <-created:
	case <-ctx.Done():
		t.Fatal("Create did not finish")
	}
	select {
	case <-reconciled:
	case <-ctx.Done():
		t.Fatal("background reconciliation did not finish")
	}
	if got.err != nil {
		t.Fatalf("Create task lost during background reconciliation (inventory revision %d -> %d): %v", actor.Revision, concurrentRevision, got.err)
	}
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil {
		t.Fatal(err)
	}
	if current.UPID != "create-task" || got.actor.UPID != current.UPID || current.OperationID != actor.OperationID || current.Stage != "create" || current.Phase != "provisioning" {
		t.Fatalf("original provider task not retained in its allocation: %+v", current)
	}
	if effects := providerEffectsOf(provider.managedFakePVE); effects.creates != 1 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("unexpected provider effects: %+v", effects)
	}
}
