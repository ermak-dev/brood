package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/workflowpolicy"
)

func TestInitialPolicyConfirmationReleasesOneActorInstructionThroughAPI(t *testing.T) {
	d := newDispatchServer(t)
	ctx := context.Background()
	swarm, err := d.srv.store.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := d.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(dispatchGraph, "role: build,", "role: build, actionClasses: [landing],", 1)
	if _, err := d.srv.store.SyncWorkflowDefinitions(ctx, swarm.ProjectID, root.ID, map[string]string{"deliver": graph}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.store.SetProjectWorkflowPolicy(ctx, root.ID, swarm.ProjectID, 0, []workflowpolicy.Rule{{ID: "land", Actions: []string{"landing"}, Require: workflowpolicy.Human}}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	d.start("deliver")
	d.mu.Lock()
	beforeAttests, beforeMessages := len(d.attests), len(d.messages)
	d.mu.Unlock()
	if beforeAttests != 0 || beforeMessages != 0 || d.committed().State != "pending" {
		t.Fatal("protected first actor started before human confirmation")
	}
	candidate, err := d.srv.store.GetPipelinePolicyCandidate(ctx, d.swarm, 1, "entry")
	if err != nil {
		t.Fatal(err)
	}
	url := "/api/swarms/" + d.swarm + "/requests/policy:" + candidate.ID + "/answer"
	for range 2 {
		r, body, _ := d.do(d.alice, "POST", url, d.csrf(d.alice), map[string]any{"response": "yes", "revision": 1})
		expectStatus(t, "initial policy confirmation", r, body, http.StatusOK)
		d.srv.steps.wait()
	}
	d.mu.Lock()
	attests, messages := len(d.attests), len(d.messages)
	d.mu.Unlock()
	if attests != 1 || messages != 1 || d.committed().State != "inflight" {
		t.Fatalf("policy response did not release exactly one instruction: attests=%d messages=%d step=%+v", attests, messages, d.committed())
	}
}
