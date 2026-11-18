package server

// BUG: Inflight история старой инкарнации оставляет новый child в очереди.
// HYPOTHESIS: Graph occupancy использовала UID без последнего шага и инкарнации.
// SOLUTION: Store применяет общий guardActorStep к текущему исполнителю.
// RESULT: FIXED — инструкция доставлена incarnation 2 после нового HTTP start.
// STATUS: FIXED

import (
	"context"
	"testing"
)

func TestChildDispatchAfterWorkerReincarnation(t *testing.T) {
	ctx := context.Background()
	d := newDispatchServer(t)
	d.srv.children.close()
	d.srv.children = newChildDispatcher(d.srv)
	d.start("deliver")
	d.stop()
	d.reincarnate(&d.worker, "build")
	d.bind("build", d.master.UID)
	d.bind("review", d.worker.UID)
	d.start("deliver")
	v := d.last()
	c, err := d.srv.managed.createChild(ctx, d.master, childCreateArguments{Sequence: v.Sequence, ParentRequestID: v.RequestID, RequestID: "after-reset", Role: "review", Task: "Review the current pass", Mode: "all", Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	d.srv.children.dispatch(ctx)
	w, err := d.srv.store.SwarmChildWorkByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Task.State != "running" || w.Task.Delivery != "delivered" || w.Task.Executor.Incarnation != 2 {
		t.Fatalf("old graph history stranded current child: %+v", w.Task)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.messages) != 3 || d.messages[2].UID != d.worker.UID || d.messages[2].Generation != 2 {
		t.Fatalf("new incarnation did not receive the child: %+v", d.messages)
	}
}
