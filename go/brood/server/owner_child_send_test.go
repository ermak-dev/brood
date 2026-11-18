package server

// BUG: A child claimed before owner intervention can still be sent after the
// interruption, despite the dispatcher seeing its durable interrupted join.
// HYPOTHESIS: The final send check verifies identity but ignores interruption.
// SOLUTION: Recheck the durable interruption flag at the final send boundary.
// RESULT: FIXED — interrupted claimed intent records not_sent without dropping history.
// STATUS: FIXED

import (
	"context"
	"testing"

	"brood.dev/go/brood/store"
)

func TestOwnerInterruptedClaimIsNotDelivered(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	c, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "claimed-before-owner"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.Task, err = d.srv.store.StartSwarmChild(ctx, w.Parent, c.RequestID, w.Task.Executor, c.Revision, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	op, err := d.srv.store.AcquireOwnerActorOperation(ctx, d.worker, "owner-control", "human", "session_terminal", d.master.DelegatorID, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := d.srv.store.FinishActorOperation(ctx, op, true); err != nil {
		t.Fatal(err)
	}
	_, before := d.calls()
	d.srv.children.deliver(ctx, w)
	_, after := d.calls()
	if len(after) != len(before) {
		t.Fatal("automatic child instruction sent after committed owner intervention")
	}
	retained, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Task.State != "running" || retained.Task.Delivery != "not_sent" || d.committed().State != "unconfirmed" {
		t.Fatalf("refused send discarded execution evidence: %+v parent=%+v", retained, d.committed())
	}
	if _, err := d.srv.store.AcquireActorOperation(ctx, d.worker.UID, d.worker.Incarnation, store.NewID(), "automatic", "restart"); err == nil {
		t.Fatal("refused send released interrupted group automatically")
	}
}
