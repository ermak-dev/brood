package server

import (
	"context"
	"fmt"
	"testing"

	"brood.dev/go/brood/store"
)

// BUG: Сохранённая старым сервером guest-selected policy обходит policy узла при join.
// HYPOTHESIS: Проверка только enqueue не ограждает уже существующую durable группу.
// SOLUTION: Join сверяет сохранённую policy со снимком узла перед продвижением.
// RESULT: FIXED — несовместимый join отказан без переписывания истории или отчётов.
// STATUS: FIXED
func TestChildJoinRejectsLegacyPolicyMismatchWithoutRewritingHistory(t *testing.T) {
	d := newChildEngineServer(t)
	parent, worker, _ := childRelay(t, d)
	ctx := context.Background()
	args := childArgs(d, "legacy")
	// Старый сервер сохранял эту policy через те же store API; сеть и join — штатные.
	for i, outcome := range []string{"success", "fail"} {
		key := fmt.Sprintf("legacy-%d", i)
		var task store.SwarmChildTask
		err := d.srv.managed.withChildParent(ctx, d.master, args.Sequence, args.ParentRequestID, func(ctx context.Context, p store.SwarmChildParent) error {
			var err error
			task, err = d.srv.store.EnqueueSwarmChild(ctx, p, store.SwarmChildJoinPolicy{Mode: "any", Total: 2, Remaining: "retain"}, key, "review", "Legacy child intent", d.srv.managed.childAuthority)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		d.srv.children.dispatch(ctx)
		if r := childCall(t, worker, "child_complete", map[string]any{"child_id": task.ID, "request_id": key, "outcome": outcome, "report": "Legacy child evidence"}); !r.OK {
			t.Fatal(r)
		}
	}
	r := childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": args.ParentRequestID, "wait_seconds": 0})
	if r.OK {
		t.Fatal("legacy guest-selected any advanced a default-all node", r)
	}
	step, err := d.srv.store.GetSwarmStep(ctx, d.swarm, 1)
	if err != nil || step.State != "inflight" || step.Outcome != nil {
		t.Fatalf("legacy mismatch changed parent: %+v %v", step, err)
	}
	history, err := d.srv.store.GetSwarmChildHistory(ctx, d.swarm, 1)
	if err != nil || history.Policy.Mode != "any" || len(history.Children) != 2 {
		t.Fatalf("legacy policy/history rewritten: %+v %v", history, err)
	}
	for _, c := range history.Children {
		if c.State != "completed" || c.Report != "Legacy child evidence" {
			t.Fatal("legacy evidence lost", c)
		}
	}
}
