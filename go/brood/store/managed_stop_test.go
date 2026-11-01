package store

import (
	"context"
	"reflect"
	"testing"
)

// BUG: Stop replaced unresolved intent and discarded its durable PVE task.
// SPEC: Reconcile an uncertain operation before replacing it; retain the whole row.
func TestManagedStopPreservesUnresolvedIntent(t *testing.T) {
	for _, phase := range []string{"provisioning", "stopping", "error"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			managedFixture(t, s)
			a, err := s.ReserveManagedActor(ctx, managedRequest("actor", "one", 105))
			if err != nil {
				t.Fatal(err)
			}
			a.Phase = "running"
			a, err = s.UpdateManagedActor(ctx, a, a.Revision)
			if err != nil {
				t.Fatal(err)
			}
			a = lifecycleConfirm(t, s, a, "running")
			a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "pending-start", "start")
			if err != nil {
				t.Fatal(err)
			}
			a.Phase = phase
			a.UPID = "UPID:unresolved-start"
			a, err = s.UpdateManagedActor(ctx, a, a.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "new-stop", "stop"); err == nil {
				t.Fatal("stop discarded unresolved operation")
			}
			got, err := s.GetManagedActor(ctx, a.UID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, a) {
				t.Fatalf("pending intent changed: %+v", got)
			}
		})
	}
}
