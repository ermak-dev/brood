package server

import (
	"context"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Ограниченное восстановление выделенных jobs по insertion cursor.
// SPEC: За проход не больше явного budget, ошибки одного job не возвращают
// предыдущим may-have-issued Create новое разрешение; конец страницы сбрасывает cursor.
func recoverAllocatedForTest(t *testing.T, d *dispatchServer, ctx context.Context, after int64, budget int) (int64, error) {
	t.Helper()
	return d.srv.recoverAllocatedEphemeralChildren(ctx, after, budget)
}

func recoverQueuedForTest(t *testing.T, d *dispatchServer, after int64, budget int, cursors map[string]int) (int64, map[string]int, error) {
	t.Helper()
	return d.srv.recoverQueuedEphemeralGroups(context.Background(), after, budget, cursors, 4)
}

func TestQueuedEphemeralRecoveryAdmitsWholeGroupWithoutEffects(t *testing.T) {
	d, intents, provider := groupCapacityFixture(t, true)
	ctx := context.Background()
	pool, err := d.srv.store.GetManagedPool(ctx, intents[0].PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	cursors := map[string]int{pool.Source.ClusterIdentity: 900}
	after, next, err := recoverQueuedForTest(t, d, 0, 1, cursors)
	if err != nil || after != intents[1].QueuePosition || len(provider.checked) != 2 || cursors[pool.Source.ClusterIdentity] != 900 || next[pool.Source.ClusterIdentity] != 902 {
		t.Fatalf("group budget, deduplication or scoped cursors failed: after=%d checked=%v cursors=%v/%v %v", after, provider.checked, cursors, next, err)
	}
	for _, intent := range intents {
		allocation, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
		if err != nil || allocation.ActorUID == "" {
			t.Fatalf("whole-group admission lost one allocation: %+v %v", allocation, err)
		}
		grant, err := d.srv.store.GetEphemeralChildCreateGrant(ctx, intent.ID)
		if err != nil || grant.State != "queued" {
			t.Fatalf("metadata recovery issued a physical grant: %+v %v", grant, err)
		}
	}
	after, _, err = recoverQueuedForTest(t, d, after, 1, next)
	if err != nil || after != 0 || len(provider.checked) != 2 {
		t.Fatalf("end cursor caused an allocation retry: after=%d checked=%v %v", after, provider.checked, err)
	}
	effects := providerEffectsOf(provider.managedFakePVE)
	if effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("queue admission reached physical effects: %+v", effects)
	}
}

func TestQueuedEphemeralRecoveryShortageLeavesEntireGroupQueued(t *testing.T) {
	d, intents, provider := groupCapacityFixture(t, true)
	ctx := context.Background()
	limit := int64(3)
	if err := d.srv.store.SetProjectCapacity(ctx, d.projectID(t), store.ProjectCapacity{MaxActors: &limit}); err != nil {
		t.Fatal(err)
	}
	before, err := d.srv.store.ListManagedActors(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := recoverQueuedForTest(t, d, 0, 1, nil)
	if err == nil || after != intents[1].QueuePosition {
		t.Fatalf("shortage was not retained with a progress cursor: after=%d %v", after, err)
	}
	actors, err := d.srv.store.ListManagedActors(ctx, d.swarm)
	if err != nil || len(actors) != len(before) {
		t.Fatalf("shortage created partial inventory: %+v %v", actors, err)
	}
	queued, err := d.srv.store.QueuedEphemeralChildIntents(ctx, 0)
	if err != nil || len(queued) != 2 {
		t.Fatalf("shortage consumed queued jobs: %+v %v", queued, err)
	}
	if providerEffectsOf(provider.managedFakePVE).creates != 0 {
		t.Fatal("shortage attempted a physical Create")
	}
}

func TestAllocatedEphemeralRecoveryBudgetCursorAndRestart(t *testing.T) {
	d, first, _, provider := ephemeralCreateFixture(t)
	addAllocatedControllerSibling(t, d, first, true)
	ctx := context.Background()
	jobs, err := d.srv.store.EphemeralChildHistoryAtStep(ctx, d.swarm, 1)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("two reserved jobs are required: %+v %v", jobs, err)
	}
	after, err := recoverAllocatedForTest(t, d, ctx, 0, 1)
	if err != nil || after != jobs[0].Intent.QueuePosition || provider.calls.Load() != 1 {
		t.Fatalf("first recovery exceeded its budget or lost cursor: after=%d calls=%d %v", after, provider.calls.Load(), err)
	}
	after, err = recoverAllocatedForTest(t, d, ctx, after, 1)
	if err != nil || after != jobs[1].Intent.QueuePosition || provider.calls.Load() != 2 {
		t.Fatalf("second recovery did not resume after first job: after=%d calls=%d %v", after, provider.calls.Load(), err)
	}
	after, err = recoverAllocatedForTest(t, d, ctx, after, 1)
	if err != nil || after != 0 || provider.calls.Load() != 2 {
		t.Fatalf("end of queue did not wrap without replay: after=%d calls=%d %v", after, provider.calls.Load(), err)
	}
	after, err = recoverAllocatedForTest(t, d, ctx, 0, 1)
	if err != nil || after != first.QueuePosition || provider.calls.Load() != 2 {
		t.Fatalf("restart replayed retained physical Create: after=%d calls=%d %v", after, provider.calls.Load(), err)
	}
}

func TestAllocatedEphemeralRecoveryHeldJobDoesNotStarveLaterWork(t *testing.T) {
	d, first, actor, provider := ephemeralCreateFixture(t)
	addAllocatedControllerSibling(t, d, first, true)
	createSQL(t, d, `UPDATE managed_actors SET operation_id='unproven-operation' WHERE uid=?`, actor.UID)
	ctx := context.Background()
	before, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := d.srv.store.EphemeralChildHistoryAtStep(ctx, d.swarm, 1)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("two jobs are required: %+v %v", jobs, err)
	}
	after, err := recoverAllocatedForTest(t, d, ctx, 0, 2)
	current, readErr := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err == nil || readErr != nil || after != jobs[1].Intent.QueuePosition || provider.calls.Load() != 1 || current != before {
		t.Fatalf("held provenance was adopted or starved the next job: after=%d calls=%d actor=%+v errors=%v/%v", after, provider.calls.Load(), current, err, readErr)
	}
	second, readErr := d.srv.store.GetManagedActor(ctx, jobs[1].Allocation.ActorUID)
	if readErr != nil || second.UPID != "create-task" {
		t.Fatalf("later admitted job was not processed: %+v %v", second, readErr)
	}
}

func TestAllocatedEphemeralRecoveryRejectsInvalidBudgetWithoutEffects(t *testing.T) {
	for _, budget := range []int{0, -1, 129} {
		d, first, _, provider := ephemeralCreateFixture(t)
		addAllocatedControllerSibling(t, d, first, true)
		if _, err := recoverAllocatedForTest(t, d, context.Background(), 0, budget); err == nil || provider.calls.Load() != 0 {
			t.Fatalf("invalid recovery budget %d reached an effect: %v", budget, err)
		}
	}
}
