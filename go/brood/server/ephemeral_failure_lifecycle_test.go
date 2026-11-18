package server

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Failure cleanup использует тот же fresh FULL stage adapter.
// SPEC: Отдельная provenance не становится report/seal; текущая authority
// и исходная allocation проверяются перед единственным physical Stop/Delete.
func TestEphemeralFailureCleanupAdapterReservesFreshStage(t *testing.T) {
	ctx := context.Background()
	d, intent, actor, pve := lifecycleServerFixture(t, "start")
	actor, err := lifecycleSubmit(d, intent, actor)
	if err != nil {
		t.Fatal(err)
	}
	actor, err = d.srv.store.ConfirmManagedActorStage(ctx, actor.UID, actor.Revision, actor.OperationID, "start", "running")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.srv.store.EnqueueEphemeralChild(ctx, intent.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second-failed-job", "Another retained task", intent.PoolUID, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []store.EphemeralChildIntent{intent, second} {
		if err := d.srv.store.RecordEphemeralChildFailure(ctx, job.ID, "Explicit local bootstrap rejection", d.srv.managed.childAuthority); err != nil {
			t.Fatal(err)
		}
	}
	join, err := d.srv.store.JoinSwarmChildren(ctx, intent.Parent, d.srv.managed.childAuthority)
	if err != nil || join.Evaluation.State != "ready_fail" || len(join.Children) != 0 {
		t.Fatalf("failure join invented child execution: %+v %v", join, err)
	}
	cleanup, err := d.srv.beginEphemeralChildLifecycle(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "cleanup")
	if err != nil {
		t.Fatal(err)
	}
	history, err := d.srv.store.GetEphemeralChildLifecycle(ctx, cleanup.UID, cleanup.Incarnation, cleanup.OperationID)
	if err != nil || history.Kind != "failure_cleanup" {
		t.Fatalf("cleanup lost distinct server-decision provenance: %+v %v", history, err)
	}
	createSQL(t, d, `CREATE TRIGGER failure_runtime_full BEFORE UPDATE OF phase ON managed_actors WHEN NEW.uid='`+actor.UID+`' AND NEW.phase='deleting' AND NEW.error='' AND NEW.upid='' AND (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'failure cleanup stage requires FULL'); END`)
	requested, err := lifecycleSubmit(d, intent, cleanup)
	if err != nil || requested.Stage != "stop" || requested.UPID != "stop-task" || pve.calls.Load() != 2 {
		t.Fatalf("explicit failure cleanup could not consume fresh FULL stop grant: %+v effects=%d %v", requested, pve.calls.Load(), err)
	}
	if _, err := lifecycleSubmit(d, intent, requested); err == nil || pve.calls.Load() != 2 {
		t.Fatalf("failure cleanup replayed a requested stop: effects=%d %v", pve.calls.Load(), err)
	}
	stopped, err := d.srv.store.ConfirmManagedActorStage(ctx, requested.UID, requested.Revision, requested.OperationID, "stop", "stopped")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := lifecycleSubmit(d, intent, stopped)
	if err != nil || deleted.Stage != "delete" || deleted.UPID != "delete-task" || deleted.Phase == "deleted" || pve.calls.Load() != 3 {
		t.Fatalf("failure cleanup delete bypassed its own fresh grant or finalized capacity: %+v effects=%d %v", deleted, pve.calls.Load(), err)
	}
	if _, err := lifecycleSubmit(d, intent, deleted); err == nil || pve.calls.Load() != 3 {
		t.Fatalf("failure cleanup replayed a requested delete: effects=%d %v", pve.calls.Load(), err)
	}
	finalized, err := d.srv.completeEphemeralChildCleanup(ctx, d.master.DelegatorID, intent.Parent, deleted.UID, deleted.Incarnation, deleted.OperationID, deleted.Revision)
	if err != nil || finalized.Phase != "deleted" || finalized.Stage != "done" || pve.calls.Load() != 3 {
		t.Fatalf("failure cleanup did not finalize from positive global absence: %+v effects=%d %v", finalized, pve.calls.Load(), err)
	}
	if _, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("failure cleanup invented an independent result seal")
	}
	retained, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID)
	if err != nil || retained.Reason != "Explicit local bootstrap rejection" {
		t.Fatalf("physical cleanup changed the logical failure: %+v %v", retained, err)
	}
}
