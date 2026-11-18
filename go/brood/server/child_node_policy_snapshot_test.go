package server

import (
	"context"
	"strings"
	"testing"
)

// BUG: Policy дочернего join не должна зависеть от нового каталога или гостя.
// HYPOTHESIS: Authority policy принадлежит снимку текущего прохода.
// SOLUTION: Enqueue использует graph роя; невозможный quorum отказывает атомарно.
// RESULT: FIXED — новый каталог не меняет policy текущего прохода; частичных записей нет.
// STATUS: FIXED
func TestChildNodePolicyPinsPassSnapshotAndRejectsImpossibleQuorum(t *testing.T) {
	d := newChildEngineServerWithPolicy(t, "mode: quorum, quorum: 2")
	parent, _, _ := childRelay(t, d)
	ctx := context.Background()
	sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := d.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(dispatchGraph, `role: build,`, `role: build, childJoin: {mode: any},`, 1)
	if _, err := d.srv.store.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{"deliver": graph}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	args := childGuestCreate(d, "impossible")
	args["mode"], args["quorum"], args["total"] = "quorum", 1, 1
	if r := childCall(t, parent, "child_create", args); r.OK {
		t.Fatal("catalog update or guest lowered current pass quorum", r)
	}
	history, err := d.srv.store.SwarmChildGroups(ctx, d.swarm, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 0 {
		t.Fatal("refused creation left a partial join", history)
	}
	args["total"] = 2
	childResponseID(t, childCall(t, parent, "child_create", args))
	history, err = d.srv.store.SwarmChildGroups(ctx, d.swarm, 0, 100)
	if err != nil || len(history.Items) != 1 || history.Items[0].Policy.Mode != "quorum" || history.Items[0].Policy.Quorum != 2 {
		t.Fatalf("pass policy changed: %+v %v", history, err)
	}
}
