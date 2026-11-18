package server

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// BUG: Generic reconcile превращает queued Create в unknown и сам запускает ephemeral.
// SPEC: Незапрошенный Create не меняется; завершённый сохраняет UPID и ждёт
// отдельного parent-fenced FULL start intent. Ни наблюдение, ни retry не дают эффект.
// SOLUTION: Точная queued-привязка не согласуется как unknown; Create не вызывает Start.
// RESULT: FIXED — runtime сохраняет UPID; автоматический Start требует отдельного FULL intent.
// STATUS: FIXED
func TestEphemeralCreateObservationKeepsUnissuedAllocation(t *testing.T) {
	d, intent, actor, provider := ephemeralCreateFixture(t)
	d.srv.managed.reconcilePass(context.Background(), actor)
	current, err := d.srv.store.GetManagedActor(context.Background(), actor.UID)
	if err != nil || current != actor {
		t.Fatalf("unissued allocation changed into uncertainty: %+v, %v", current, err)
	}
	grant, err := d.srv.store.GetEphemeralChildCreateGrant(context.Background(), intent.ID)
	if err != nil || grant.State != "queued" {
		t.Fatalf("queued permission changed: %+v %v", grant, err)
	}
	if effects := providerEffectsOf(provider.managedFakePVE); effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("observation sent work: %+v", effects)
	}
}

func TestEphemeralCreateObservationDoesNotStartGuest(t *testing.T) {
	ctx := context.Background()
	d, intent, _, provider := ephemeralCreateFixture(t)
	actor, err := d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		d.srv.managed.reconcilePass(ctx, actor)
		current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Operation != "create" || current.OperationID != actor.OperationID || current.Stage != "done" || current.Phase != "stopped" || current.UPID != "create-task" || current.Desired != "running" {
			t.Fatalf("Create observation minted or started another operation: %+v", current)
		}
		actor = current
	}
	if effects := providerEffectsOf(provider.managedFakePVE); effects.creates != 1 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("Create observation bypassed independent FULL start permission: %+v", effects)
	}
	started, err := d.srv.beginEphemeralChildLifecycle(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "start")
	if err != nil {
		t.Fatal(err)
	}
	started, err = d.srv.submitEphemeralChildLifecycleStage(ctx, d.master.DelegatorID, intent.Parent, started.UID, started.Incarnation, started.OperationID, "start", started.Revision)
	if err != nil || started.UPID != "start-task" || started.OperationID == actor.OperationID {
		t.Fatalf("separate authorized start cannot proceed after observation: %+v %v", started, err)
	}
	if effects := providerEffectsOf(provider.managedFakePVE); effects.creates != 1 || effects.starts != 1 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("separate authorized start count: %+v", effects)
	}
}

func TestEphemeralCreateObservationUnknownDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost-reply", true: "pending-task"}[bound], func(t *testing.T) {
			d, intent, actor, provider := ephemeralCreateFixture(t)
			provider.uncertain = !bound
			if bound {
				provider.taskErrors = map[string]error{"create-task": proxmox.ErrUncertain}
			}
			current, err := d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !bound && (current.UPID != "" || current.Error == "") {
				t.Fatalf("uncertain submission was not retained: %+v", current)
			}
			if current.UID != "" {
				actor = current
			}
			for pass := 0; pass < 2; pass++ {
				d.srv.managed.reconcilePass(ctx, actor)
				actor, err = d.srv.store.GetManagedActor(ctx, actor.UID)
				if err != nil {
					t.Fatal(err)
				}
			}
			grant, err := d.srv.store.GetEphemeralChildCreateGrant(ctx, intent.ID)
			if err != nil || grant.State != "reserved" || actor.Stage != "create" {
				t.Fatalf("unknown work released or advanced: %+v %+v %v", actor, grant, err)
			}
			if effects := providerEffectsOf(provider.managedFakePVE); effects.creates != 1 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
				t.Fatalf("unknown work repeated: %+v", effects)
			}
			if bound && actor.UPID != "create-task" {
				t.Fatal("bound task was discarded")
			}
		})
	}
}

func TestEphemeralCreateObservationTerminalFailureRetainsCapacity(t *testing.T) {
	ctx := context.Background()
	d, intent, _, provider := ephemeralCreateFixture(t)
	actor, err := d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider.taskErrors = map[string]error{"create-task": proxmox.ErrTaskFailed}
	d.srv.managed.reconcilePass(ctx, actor)
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current.Stage != "done" || current.Phase != "error" || current.UPID != actor.UPID || current.DeletedAt != nil {
		t.Fatalf("failed Create lost task or reservation: %+v %v", current, err)
	}
	if _, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("observation invented a logical failure: %v", err)
	}
	if effects := providerEffectsOf(provider.managedFakePVE); effects.creates != 1 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("failure observation sent work: %+v", effects)
	}
}
