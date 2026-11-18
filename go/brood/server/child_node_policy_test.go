package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// BUG: Гость задаёт policy вместо actor-узла и подменяет исход графа.
// HYPOTHESIS: createChild сохраняет args.Mode/Quorum без снимка определения узла.
// SOLUTION: Сервер читает childJoin из снимка graph в транзакции создания ребёнка.
// RESULT: FIXED — реальные relay-команды не меняют policy и quorum узла.
// STATUS: FIXED
func newChildEngineServerWithPolicy(t *testing.T, policy string) *dispatchServer {
	t.Helper()
	d := newDispatchServer(t)
	d.srv.children.close()
	d.srv.children = newChildDispatcher(d.srv)
	d.bind("build", d.master.UID)
	d.bind("review", d.worker.UID)
	ctx := context.Background()
	sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := d.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(dispatchGraph, `role: build,`, `role: build, childJoin: {`+policy+`},`, 1)
	if _, err := d.srv.store.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{"deliver": graph}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	d.start("deliver")
	return d
}

func TestChildNodePolicyControlsOutcomeOverGuestArguments(t *testing.T) {
	for _, tc := range []struct {
		name, policy, guestMode, outcome string
		guestQuorum                      int
	}{
		{"node_any_guest_all", "mode: any", "all", "success", 0},
		{"node_quorum_guest_lower_threshold", "mode: quorum, quorum: 2", "quorum", "fail", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newChildEngineServerWithPolicy(t, tc.policy)
			parent, worker, _ := childRelay(t, d)
			for i, outcome := range []string{"success", "fail"} {
				key := fmt.Sprintf("child-%d", i)
				args := childGuestCreate(d, key)
				args["mode"], args["quorum"] = tc.guestMode, tc.guestQuorum
				id := childResponseID(t, childCall(t, parent, "child_create", args))
				if again := childResponseID(t, childCall(t, parent, "child_create", args)); again != id {
					t.Fatal("retry duplicated child")
				}
				d.srv.children.dispatch(context.Background())
				if r := childCall(t, worker, "child_complete", map[string]any{"child_id": id, "request_id": key, "outcome": outcome, "report": "Child report"}); !r.OK {
					t.Fatal(r)
				}
			}
			r := childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": childGuestCreate(d, "unused")["parent_request_id"], "wait_seconds": 0})
			if !r.OK || r.Outcome != "completed" {
				t.Fatal(r)
			}
			step, err := d.srv.store.GetSwarmStep(context.Background(), d.swarm, 1)
			if err != nil || step.Outcome == nil || *step.Outcome != tc.outcome {
				t.Fatalf("node policy lost: step=%+v error=%v", step, err)
			}
		})
	}
}
