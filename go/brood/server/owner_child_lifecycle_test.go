package server

// BUG: Explicit owner lifecycle requests inherited automatic child holds.
// HYPOTHESIS: Managed workload intent did not distinguish human intervention.
// SOLUTION: Commit owner intervention evidence with lifecycle intent.

import (
	"context"
	"net/http"
	"testing"

	"brood.dev/go/brood/store"
)

func TestOwnerChildLifecycleHTTP(t *testing.T) {
	for _, action := range []string{"stop", "reset"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			d := newChildEngineServer(t)
			c, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "running"))
			if err != nil {
				t.Fatal(err)
			}
			d.srv.children.dispatch(ctx)
			r, b, _ := d.do(d.alice, "POST", "/api/swarms/"+d.swarm+"/actors/"+d.worker.Name, d.csrf(d.alice), map[string]string{"action": action})
			expectStatus(t, "explicit owner "+action, r, b, http.StatusOK)
			w, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if w.Task.State != "running" || d.committed().State != "unconfirmed" {
				t.Fatalf("lifecycle orphaned child or concealed interruption: %+v parent=%+v", w, d.committed())
			}
			a, err := d.srv.store.GetManagedActor(ctx, d.worker.UID)
			if err != nil {
				t.Fatal(err)
			}
			if a.Desired != map[string]string{"stop": "stopped", "reset": "running"}[action] || a.Operation != action {
				t.Fatalf("owner intent missing: %+v", a)
			}
			if err := d.srv.managed.completeChild(ctx, d.worker, c.ID, c.RequestID, "success", "late old incarnation report"); err == nil {
				t.Fatal("late stopped/reset completion accepted")
			}
			retained, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
			if err != nil || retained.Task.State != "running" {
				t.Fatalf("stale completion removed running evidence: %+v %v", retained, err)
			}
			// An automatic actor workspace operation remains refused after intervention.
			if _, err := d.srv.store.AcquireActorOperation(ctx, a.UID, a.Incarnation, store.NewID(), "automatic", "restart"); err == nil {
				t.Fatal("automatic mutation crossed retained execution hold")
			}
		})
	}
}
