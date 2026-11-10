package server

// FEATURE: bro-клиент запуска и остановки роя против настоящего сервера (#242).
// SPEC: clientapi запускает проход с задачей и останавливает его; причина
// каждого отказа запуска и остановки доходит до вызывающего клиента вместе
// со своей подробностью (состояние, определение, роль). Предел задачи у
// клиента и у сервера один.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/clientapi"
	"brood.dev/go/brood/store"
)

// expectReasonPrefix — отказ клиента несёт публичную причину сервера целиком,
// начиная с её фиксированной части.
func expectReasonPrefix(t *testing.T, what string, err error, prefix string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), ": "+prefix) {
		t.Fatalf("%s: %v, want a reason starting with %q", what, err, prefix)
	}
}

func TestSwarmStartThroughClient(t *testing.T) {
	if clientapi.MaxSwarmGoalBytes != store.MaxSwarmGoalBytes {
		t.Fatalf("client goal limit %d, server %d", clientapi.MaxSwarmGoalBytes, store.MaxSwarmGoalBytes)
	}
	ctx := context.Background()
	f := startAPISetup(t)
	alice, bob := roleClient(t, f, "alice"), roleClient(t, f, "bob")
	release := clientapi.SwarmStart{Definition: "release", Goal: startAPIGoal, Commit: startAPICommit}

	_, err := bob.StartSwarm(ctx, f.swarm, release)
	expectReason(t, "outsider start", err, "swarm not found")
	r, b, _ := f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/members", f.ts.csrf(f.alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, http.StatusNoContent)
	_, err = bob.StartSwarm(ctx, f.swarm, release)
	expectReason(t, "viewer start", err, "swarm management denied")

	for _, tc := range []struct {
		what   string
		start  clientapi.SwarmStart
		reason string
	}{
		{"missing definition", clientapi.SwarmStart{Definition: "absent", Goal: startAPIGoal, Commit: startAPICommit}, `workflow definition is not in the project catalog: "absent"`},
		{"blank goal", clientapi.SwarmStart{Definition: "release", Goal: " \n", Commit: startAPICommit}, "invalid swarm goal: the goal is empty"},
		{"goal with the envelope tag", clientapi.SwarmStart{Definition: "release", Goal: "a " + envelopeClosingTag, Commit: startAPICommit}, "invalid swarm goal: the goal contains the message envelope closing tag"},
		{"goal over the limit", clientapi.SwarmStart{Definition: "release", Goal: strings.Repeat("x", clientapi.MaxSwarmGoalBytes+1), Commit: startAPICommit}, "invalid swarm goal: the goal exceeds 4096 bytes"},
		{"commit without server Git access", clientapi.SwarmStart{Definition: "release", Goal: startAPIGoal}, "cannot resolve the project branch: the project repository has no server Git access; bind a project Git credential or name the commit"},
	} {
		_, err := alice.StartSwarm(ctx, f.swarm, tc.start)
		expectReason(t, tc.what, err, tc.reason)
	}
	_, err = alice.StartSwarm(ctx, f.swarm, clientapi.SwarmStart{Definition: "review", Goal: startAPIGoal, Commit: startAPICommit})
	expectReasonPrefix(t, "unbound role", err, `role "review": `)
	_, err = alice.StartSwarm(ctx, f.swarm, clientapi.SwarmStart{Definition: "retrying", Goal: startAPIGoal, Commit: startAPICommit})
	expectReasonPrefix(t, "graph the runner refuses", err, `workflow graph is not executable: workflow "retrying": `)

	started, err := alice.StartSwarm(ctx, f.swarm, release)
	if err != nil || started.Execution != "waiting" || started.Goal != startAPIGoal || started.Step == nil || started.Step.Sequence != 1 || started.Step.State != "waiting" || started.Source == nil || started.Source.Commit != startAPICommit || started.Definition == nil || started.Definition.Name != "release" {
		t.Fatalf("start: %+v %v", started, err)
	}
	_, err = alice.StartSwarm(ctx, f.swarm, release)
	expectReason(t, "second start", err, "swarm state conflict: execution is waiting; start requires idle, succeeded, failed or stopped")
	_, err = bob.StopSwarm(ctx, f.swarm)
	expectReason(t, "viewer stop", err, "swarm management denied")
	stopped, err := alice.StopSwarm(ctx, f.swarm)
	if err != nil || stopped.Execution != "stopped" || stopped.Goal != startAPIGoal || stopped.Step == nil || stopped.Step.State != "stopped" {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	_, err = alice.StopSwarm(ctx, f.swarm)
	expectReason(t, "repeated stop", err, "swarm state conflict: execution is stopped; stop requires running or waiting")
	_, err = alice.StartSwarm(ctx, f.swarm, clientapi.SwarmStart{Definition: "approve", Goal: startAPIGoal})
	expectReasonPrefix(t, "another definition", err, `swarm graph is pinned: the swarm runs workflow "release"`)
	again, err := alice.StartSwarm(ctx, f.swarm, clientapi.SwarmStart{Definition: "release", Goal: "Second pass."})
	if err != nil || again.Goal != "Second pass." || again.Step == nil || again.Step.Sequence != 2 {
		t.Fatalf("restart: %+v %v", again, err)
	}
}
