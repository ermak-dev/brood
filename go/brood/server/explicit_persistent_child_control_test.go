package server

import (
	"context"
	"fmt"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Явный parent управляет своим persistent worker при child hold.
// SPEC: Команда сохраняет child/report и прерывает точный parent до эффекта;
// automatic lifecycle остаётся запрещён, повтор физической операции не выдаётся.
func TestExplicitChildGuestLifecycleRetainsInterruptedWork(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{"stop", "reset", "delete"} {
		t.Run(action, func(t *testing.T) {
			d := newOwnedChildEngineServer(t)
			parent, _, _ := childRelay(t, d)
			id := childResponseID(t, childCall(t, parent, "child_create", childGuestCreate(d, "one")))
			d.srv.children.dispatch(ctx)
			before, err := d.srv.store.SwarmChildWorkByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"name": d.worker.Name}
			if action != "stop" {
				args["confirm"] = true
			}
			r := childCall(t, parent, action, args)
			if !r.OK || r.Outcome != "accepted" {
				t.Fatalf("explicit parent %s blocked by automatic child hold: %+v", action, r)
			}
			current, err := d.srv.store.GetManagedActor(ctx, d.worker.UID)
			if err != nil || current.Incarnation != d.worker.Incarnation || current.CTID != d.worker.CTID || current.Operation != action || current.Stage != "stop" || current.UPID == "" {
				t.Fatalf("explicit lifecycle lost physical identity/task: %+v %v", current, err)
			}
			origin, err := d.srv.store.GetOwnerManagedActorOperation(ctx, current.UID, current.Incarnation, current.OperationID)
			if err != nil || origin.Caller != fmt.Sprintf("actor:%s:%d", d.master.UID, d.master.Incarnation) || origin.Operation != action || origin.State != "completed" {
				t.Fatalf("explicit lifecycle lost truthful caller: %+v %v", origin, err)
			}
			after, err := d.srv.store.SwarmChildWorkByID(ctx, id)
			if err != nil || after.Task != before.Task {
				t.Fatalf("explicit control rewrote child/report/transport: %+v %v", after, err)
			}
			join, err := d.srv.store.GetSwarmChildJoin(ctx, before.Parent, d.srv.managed.childAuthority)
			if err != nil || !join.Interrupted || join.Evaluation.State != "unconfirmed" {
				t.Fatalf("explicit control lost parent uncertainty: %+v %v", join, err)
			}
			if err := d.srv.store.CheckActorChildObligations(ctx, current.UID); err == nil {
				t.Fatal("explicit control released automatic child hold")
			}
			if _, err := d.srv.store.BeginManagedActorOperation(ctx, current.UID, current.Revision, store.NewID(), "delete"); err == nil {
				t.Fatal("automatic lifecycle acquired replacement authority")
			}
		})
	}
}
