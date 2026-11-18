package server

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Продвижение выделенного executor без частичного старта fanout.
// SPEC: Все логические слоты группы зарезервированы до первого Create;
// повторный проход не повторяет may-have-issued запрос и не меняет очередь.
// IMPLEMENTATION: Один переход через отдельные FULL adapters; applied join
// предшествует seal/cleanup, positive absence освобождает reservation.
// RESULT: IMPLEMENTED — полный Go/Python прогон и focused race проходят.
// STATUS: DONE
func advanceAllocatedChildForTest(t *testing.T, d *dispatchServer, id string) error {
	t.Helper()
	return d.srv.advanceAllocatedEphemeralChild(context.Background(), id)
}

func addAllocatedControllerSibling(t *testing.T, d *dispatchServer, first store.EphemeralChildIntent, allocate bool) {
	t.Helper()
	ctx := context.Background()
	second, err := d.srv.store.EnqueueEphemeralChild(ctx, first.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second-isolated", "Second isolated task", first.PoolUID, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if !allocate {
		return
	}
	pool, err := d.srv.store.GetManagedPool(ctx, first.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := d.srv.managed.registry.pools[pool.Connection].runtime
	runtime.actors = pool.MaxActors
	runtime.cpuMillicores = pool.Resources.CPUMillicores
	runtime.memoryBytes = pool.Resources.MemoryBytes
	runtime.diskGiB = pool.Resources.DiskBytes / gib
	_, err = d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, second.Parent, second.ID, store.EphemeralActorReservation{UID: "second-isolated-worker", OwnerNonce: "second-isolated-nonce", OperationID: "second-isolated-create", CTID: 910, Resources: d.master.Resources})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAllocatedEphemeralControllerFinalizesBoundStopAfterAbsence(t *testing.T) {
	d, intent, actor, provider := lifecycleServerFixture(t, "stop")
	if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	current, err := d.srv.store.GetManagedActor(context.Background(), actor.UID)
	if err != nil || current.UPID != "stop-task" || current.Stage != "stop" || provider.calls.Load() != 1 {
		t.Fatalf("fixture did not retain a bound Stop: %+v %v", current, err)
	}
	provider.mu.Lock()
	provider.exists = false
	provider.running = false
	provider.mu.Unlock()
	if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	current, err = d.srv.store.GetManagedActor(context.Background(), actor.UID)
	if err != nil || current.Phase != "deleted" || current.DeletedAt == nil || provider.calls.Load() != 1 {
		t.Fatalf("successful bound Stop and positive absence did not release reservation: %+v calls=%d %v", current, provider.calls.Load(), err)
	}
}

func TestAllocatedEphemeralControllerDoesNotReleaseUnknownCleanup(t *testing.T) {
	d, intent, actor, provider := lifecycleServerFixture(t, "delete")
	provider.lost = true
	_ = advanceAllocatedChildForTest(t, d, intent.ID)
	ctx := context.Background()
	retained, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	provider.mu.Lock()
	physicallyAbsent := !provider.exists
	provider.mu.Unlock()
	if err != nil || !physicallyAbsent || retained.Phase == "deleted" || retained.DeletedAt != nil || retained.UPID != "" || retained.Error == "" || deleteNotRequested(retained) || provider.calls.Load() != 1 {
		t.Fatalf("unknown Delete released its reservation: %+v %v", retained, err)
	}
	beforeAbsence := provider.absenceChecks
	if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current != retained || provider.calls.Load() != 1 || provider.absenceChecks != beforeAbsence {
		t.Fatalf("physical absence became permission after lost Delete: %+v %v", current, err)
	}
}

func TestAllocatedEphemeralControllerRechecksCurrentDelegation(t *testing.T) {
	d, intent, actor, provider := ephemeralCreateFixture(t)
	addAllocatedControllerSibling(t, d, intent, true)
	ctx := context.Background()
	if err := d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true); err != nil {
		t.Fatal(err)
	}
	if err := advanceAllocatedChildForTest(t, d, intent.ID); err == nil {
		t.Fatal("revoked delegator authorized a Create")
	}
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	grant, grantErr := d.srv.store.GetEphemeralChildCreateGrant(ctx, intent.ID)
	if err != nil || grantErr != nil || current != actor || grant.State != "queued" || provider.calls.Load() != 0 {
		t.Fatalf("revocation changed the retained queued slot: %+v grant=%+v %v/%v", current, grant, err, grantErr)
	}
}

func TestAllocatedEphemeralControllerFinalizesKnownUnsentAbsentCleanup(t *testing.T) {
	for _, stage := range []string{"stop", "delete"} {
		t.Run(stage, func(t *testing.T) {
			d, intent, actor, provider := lifecycleServerFixture(t, stage)
			provider.mu.Lock()
			provider.exists = false
			provider.running = false
			provider.mu.Unlock()
			if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			current, err := d.srv.store.GetManagedActor(context.Background(), actor.UID)
			if err != nil || current.Phase != "deleted" || current.DeletedAt == nil || provider.calls.Load() != 0 || provider.absenceChecks != 1 {
				t.Fatalf("known-unrequested absent %s did not finalize without effects: %+v calls=%d absence=%d %v", stage, current, provider.calls.Load(), provider.absenceChecks, err)
			}
		})
	}
}

func TestAllocatedEphemeralControllerSealsOnlyAfterAppliedJoin(t *testing.T) {
	ctx := context.Background()
	d, actor, child := runningEphemeralChildReportFixture(t)
	intent, err := d.srv.store.GetEphemeralChildIntentForTask(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.srv.store.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision, "success", "Independent executor report", d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	second, err := d.srv.store.EnqueueSwarmChild(ctx, intent.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "cancelled-sibling", "review", "Cancelled sibling", d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.srv.store.CancelSwarmChild(ctx, intent.Parent, second.RequestID, second.Revision, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	if err = advanceAllocatedChildForTest(t, d, intent.ID); err == nil {
		t.Fatal("missing applied aggregate join must carry a refusal reason")
	}
	if _, err = d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("controller sealed before the parent applied its computed join")
	}
	if _, err = d.srv.store.JoinSwarmChildren(ctx, intent.Parent, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	seal, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID)
	current, readErr := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || readErr != nil || seal.ChildID != child.ID || current != actor {
		t.Fatalf("applied join did not retain independent result before cleanup: %+v actor=%+v errors=%v/%v", seal, current, err, readErr)
	}
	if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	current, err = d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current.Operation != "delete" || current.Stage != "stop" || !stopNotRequested(current) {
		t.Fatalf("retained result did not begin a separate unsent cleanup: %+v %v", current, err)
	}
}

func TestAllocatedEphemeralControllerFailureCleanupRequiresAppliedJoin(t *testing.T) {
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
	second, err := d.srv.store.EnqueueEphemeralChild(ctx, intent.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second-failed", "Second failed task", intent.PoolUID, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []store.EphemeralChildIntent{intent, second} {
		if err := d.srv.store.RecordEphemeralChildFailure(ctx, job.ID, "Explicit trusted failure", d.srv.managed.childAuthority); err != nil {
			t.Fatal(err)
		}
	}
	beforeEffects := provider.calls.Load()
	if err := advanceAllocatedChildForTest(t, d, intent.ID); err == nil {
		t.Fatal("failure cleanup must be held before aggregate join")
	}
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current != actor || provider.calls.Load() != beforeEffects {
		t.Fatalf("failure before aggregate join changed physical state: %+v %v", current, err)
	}
	if _, err := d.srv.store.JoinSwarmChildren(ctx, intent.Parent, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	current, err = d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current.Operation != "delete" || current.Stage != "stop" || !stopNotRequested(current) || provider.calls.Load() != beforeEffects {
		t.Fatalf("applied failed join did not begin separate unsent cleanup: %+v %v", current, err)
	}
	if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != beforeEffects+1 {
		t.Fatal("failure cleanup did not consume exactly one Stop grant")
	}
	if _, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("controller invented a report seal for a server failure")
	}
}

func TestAllocatedEphemeralControllerAttestsThenRegistersOnce(t *testing.T) {
	d, intent, actor, _ := lifecycleServerFixture(t, "start")
	addAllocatedControllerSibling(t, d, intent, true)
	ctx := context.Background()
	actor, err := lifecycleSubmit(d, intent, actor)
	if err != nil {
		t.Fatal(err)
	}
	actor, err = d.srv.store.ConfirmManagedActorStage(ctx, actor.UID, actor.Revision, actor.OperationID, "start", "running")
	if err != nil {
		t.Fatal(err)
	}
	d.onConnected(func(base commandSessionHub, uid string, incarnation int64) bool {
		return uid == actor.UID && incarnation == actor.Incarnation || base.Connected(uid, incarnation)
	})
	beforeCalls, _ := d.calls()
	if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	proof, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID)
	if err != nil || proof.State != "attested" || proof.AttestedCommit != intent.SourceCommit {
		t.Fatalf("running executor did not retain source proof: %+v %v", proof, err)
	}
	if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	history, err := d.srv.store.GetEphemeralChildHistory(ctx, intent.ID)
	afterCalls, _ := d.calls()
	if err != nil || history.ChildID == "" || len(afterCalls)-len(beforeCalls) != 1 {
		t.Fatalf("attested executor did not register without another root RPC: %+v calls=%d %v", history, len(afterCalls)-len(beforeCalls), err)
	}
}

func TestAllocatedEphemeralControllerPreparesTypedInstruction(t *testing.T) {
	d, intent, child, _, _ := childDeliveryFixture(t)
	ctx := context.Background()
	second, err := d.srv.store.EnqueueSwarmChild(ctx, intent.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "cancelled-sibling", "review", "Cancelled sibling", d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.srv.store.CancelSwarmChild(ctx, intent.Parent, second.RequestID, second.Revision, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	delivery, err := d.srv.store.GetActorDeliveryForSource(ctx, d.swarm, "child", child.ID)
	if err != nil || delivery.Input.SourceID != child.ID || delivery.Input.RequestID != intent.RequestID || delivery.Input.ActorUID != child.Executor.ActorUID || delivery.Input.Payload == "" {
		t.Fatalf("registered executor did not prepare its exact typed instruction: %+v %v", delivery, err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil || work.Task.State != "running" || work.Task.Report != "" || work.Task.Outcome != "" {
		t.Fatalf("instruction preparation invented a result: %+v %v", work, err)
	}
	if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := d.srv.store.GetActorDeliveryForSource(ctx, d.swarm, "child", child.ID)
	if err != nil || !reflect.DeepEqual(delivery, retry) {
		t.Fatalf("controller rewrote the retained instruction: %+v %v", retry, err)
	}
}

func TestAllocatedEphemeralControllerFreshLifecycleStages(t *testing.T) {
	for _, stage := range []string{"start", "stop", "delete"} {
		t.Run(stage, func(t *testing.T) {
			d, intent, actor, provider := lifecycleServerFixture(t, stage)
			if stage == "start" {
				addAllocatedControllerSibling(t, d, intent, true)
			}
			if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			current, err := d.srv.store.GetManagedActor(context.Background(), actor.UID)
			if err != nil || current.UPID != stage+"-task" || provider.calls.Load() != 1 {
				t.Fatalf("fresh %s did not consume exactly one stage grant: %+v calls=%d %v", stage, current, provider.calls.Load(), err)
			}
			if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			if provider.calls.Load() != 1 {
				t.Fatalf("controller replayed %s", stage)
			}
			if stage == "delete" {
				current, err = d.srv.store.GetManagedActor(context.Background(), actor.UID)
				if err != nil || current.Phase != "deleted" || current.DeletedAt == nil {
					t.Fatalf("bound cleanup did not reach positive-absence finalizer: %+v %v", current, err)
				}
			}
		})
	}
}

func TestAllocatedEphemeralControllerBeginsStartWithoutSubmittingIt(t *testing.T) {
	d, intent, actor, provider := ephemeralCreateFixture(t)
	addAllocatedControllerSibling(t, d, intent, true)
	ctx := context.Background()
	var err error
	actor, err = d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	actor, err = d.srv.store.ConfirmManagedActorStage(ctx, actor.UID, actor.Revision, actor.OperationID, "create", "stopped")
	if err != nil {
		t.Fatal(err)
	}
	if err = advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
		t.Fatal(err)
	}
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current.Stage != "start" || current.OperationID == actor.OperationID || current.UPID != "" || !startNotRequested(current) || provider.calls.Load() != 1 {
		t.Fatalf("Create completion did not begin a separate unsent Start: %+v calls=%d %v", current, provider.calls.Load(), err)
	}
}

func TestAllocatedEphemeralControllerRequiresWholeGroupReservation(t *testing.T) {
	for _, scenario := range []string{"missing-member", "unallocated-member", "allocated-group", "unknown-create"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, actor, provider := ephemeralCreateFixture(t)
			if scenario != "missing-member" {
				addAllocatedControllerSibling(t, d, intent, scenario != "unallocated-member")
			}
			if scenario == "unknown-create" {
				provider.uncertain = true
			}
			before, err := d.srv.store.EphemeralChildHistoryAtStep(context.Background(), d.swarm, 1)
			if err != nil {
				t.Fatal(err)
			}
			err = advanceAllocatedChildForTest(t, d, intent.ID)
			current, readErr := d.srv.store.GetManagedActor(context.Background(), actor.UID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "missing-member" || scenario == "unallocated-member" {
				after, readErr := d.srv.store.EphemeralChildHistoryAtStep(context.Background(), d.swarm, 1)
				if !errors.Is(err, store.ErrSwarmChildConflict) || readErr != nil || !reflect.DeepEqual(before, after) || current != actor || provider.calls.Load() != 0 {
					t.Fatalf("partial group advanced: actor=%+v calls=%d err=%v history=%v", current, provider.calls.Load(), err, readErr)
				}
				return
			}
			if scenario == "allocated-group" && (err != nil || current.UPID != "create-task") {
				t.Fatalf("fully reserved group did not submit Create: %+v %v", current, err)
			}
			if provider.calls.Load() != 1 {
				t.Fatalf("expected exactly one Create, got %d: %v", provider.calls.Load(), err)
			}
			retained := current
			_ = advanceAllocatedChildForTest(t, d, intent.ID)
			current, readErr = d.srv.store.GetManagedActor(context.Background(), actor.UID)
			if readErr != nil || current != retained || provider.calls.Load() != 1 {
				t.Fatalf("controller replayed a retained Create: %+v calls=%d %v", current, provider.calls.Load(), readErr)
			}
		})
	}
}
