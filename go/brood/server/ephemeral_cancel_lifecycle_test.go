package server

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Cancel-before-instruction cleanup сохраняет отдельную provenance.
// SPEC: После exact applied join отменённый pending child очищается без report,
// seal или failure; unknown Delete не повторяется и не освобождает capacity.
func TestEphemeralCancelledChildLifecycleAfterJoin(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "unknown-delete"}[unknown], func(t *testing.T) {
			ctx := context.Background()
			d, intent, actor, provider := lifecycleServerFixture(t, "start")
			actor, err := lifecycleSubmit(d, intent, actor)
			if err != nil {
				t.Fatal(err)
			}
			actor, err = d.srv.store.ConfirmManagedActorStage(ctx, actor.UID, actor.Revision, actor.OperationID, "start", "running")
			if err != nil {
				t.Fatal(err)
			}
			d.onConnected(func(base commandSessionHub, uid string, inc int64) bool {
				return uid == actor.UID && inc == actor.Incarnation || base.Connected(uid, inc)
			})
			if _, err = d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "cancel-source-proof"); err != nil {
				t.Fatal(err)
			}
			child, err := d.srv.registerEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			child, err = d.srv.store.CancelSwarmChild(ctx, intent.Parent, child.RequestID, child.Revision, d.srv.managed.childAuthority)
			if err != nil {
				t.Fatal(err)
			}
			second, err := d.srv.store.EnqueueSwarmChild(ctx, intent.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "cancelled-sibling", "review", "Cancelled sibling", d.srv.managed.childAuthority)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.srv.store.CancelSwarmChild(ctx, intent.Parent, second.RequestID, second.Revision, d.srv.managed.childAuthority); err != nil {
				t.Fatal(err)
			}
			beforeEffects := provider.calls.Load()
			_, beforeMessages := d.calls()
			if _, err = d.srv.store.JoinSwarmChildren(ctx, intent.Parent, d.srv.managed.childAuthority); err != nil {
				t.Fatal(err)
			}
			if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			actor, err = d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || actor.Operation != "delete" || !stopNotRequested(actor) || provider.calls.Load() != beforeEffects {
				t.Fatalf("cancelled no-execution child did not begin a distinct unsent cleanup: %+v %v", actor, err)
			}
			history, err := d.srv.store.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
			if err != nil || history.Kind != "cancel_cleanup" {
				t.Fatalf("cancelled task lost dedicated provenance: %+v %v", history, err)
			}
			if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			actor, err = d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || actor.UPID != "stop-task" {
				t.Fatalf("cancel cleanup could not reserve its Stop: %+v %v", actor, err)
			}
			d.srv.managed.lifecycle.Lock()
			d.srv.managed.reconcilePass(ctx, actor)
			d.srv.managed.lifecycle.Unlock()
			provider.lost = unknown
			if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			actor, err = d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || (actor.Phase == "deleted") == unknown || (actor.DeletedAt != nil) == unknown || provider.calls.Load() != beforeEffects+2 {
				t.Fatalf("cancel cleanup repeated effects or lost absence/unknown boundary: %+v calls=%d %v", actor, provider.calls.Load(), err)
			}
			work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
			_, afterMessages := d.calls()
			if err != nil || work.Task.State != "cancelled" || work.Task.Report != "" || work.Task.Outcome != "" || len(afterMessages) != len(beforeMessages) {
				t.Fatalf("cancel cleanup fabricated execution or an independent report: %+v %v", work, err)
			}
			if _, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("cancel cleanup invented a seal")
			}
			if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("cancel cleanup invented failure")
			}
		})
	}
}
