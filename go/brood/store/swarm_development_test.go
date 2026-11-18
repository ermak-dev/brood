package store

import (
	"context"
	"fmt"
	"os"
	"testing"

	"brood.dev/go/brood/build"
	"gopkg.in/yaml.v3"
)

// TestLegacySwarmWorkflowProgression preserves a pinned architecture graph after
// catalog replacement: both decision answers land in the right visit and status.
// Legacy runs here deliberately have no source snapshot; source-bound execution
// stays fenced.
func TestLegacySwarmWorkflowProgression(t *testing.T) {
	data, err := os.ReadFile("../build/testdata/legacy-swarm-workflows.Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := build.ParseDocument(string(data), "legacy-swarm-workflows.Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	graph, err := build.Workflow(doc, "swarm-architecture", "legacy-swarm-workflows.Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	options := make(map[string]string)
	for _, option := range graph.Nodes["decision"].Options {
		options[option.Label] = option.Outcome
	}
	if len(options) != 2 || options["Approve"] != "success" || options["Reject"] != "fail" {
		t.Fatalf("decision options: %+v", options)
	}
	snapshot, err := yaml.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	before := []string{"triage", "architecture"}
	after := []string{"implement", "review", "test", "integrate"}
	for _, tc := range []struct{ answer, status string }{
		{"success", "succeeded"},
		{"fail", "failed"},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			ctx := context.Background()
			s := sourceTestStore(t)
			if err := attachTestGraph(ctx, s, "s", string(snapshot)); err != nil {
				t.Fatal(err)
			}
			// Две роли графа — два актора роя; каждый шаг адресует актора
			// роли своего узла (#208).
			actors := map[string]string{"review": "reviewer", "build": "builder"}
			for role, uid := range actors {
				roleActor(t, s, "s", uid, role)
			}
			sequence := int64(0)
			complete := func(node string) {
				t.Helper()
				sequence++
				request := fmt.Sprintf("request-%d", sequence)
				actor := actors[graph.Nodes[node].Role]
				if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: node, State: "inflight", ActorUID: actor, ActorIncarnation: 1, RequestID: request}); err != nil {
					t.Fatal(err)
				}
				if err := s.CompletePipelineActorVisit(ctx, "s", "s", sequence, request, actor, 1, "success", ""); err != nil {
					t.Fatal(err)
				}
			}
			for _, node := range before {
				complete(node)
			}
			sw, err := s.GetSwarm(ctx, "s")
			visits, visitsErr := s.SwarmSteps(ctx, "s")
			if err != nil || visitsErr != nil || sw.Execution != "waiting" || len(visits) != int(sequence)+1 || visits[sequence].NodeID != "decision" || visits[sequence].State != "waiting" {
				t.Fatalf("gated run: swarm=%+v visits=%+v errors=%v, %v", sw, visits, err, visitsErr)
			}
			sequence++
			if err := s.AnswerPipelineVisit(ctx, "s", "s", sequence, tc.answer); err != nil {
				t.Fatal(err)
			}
			if tc.answer == "fail" {
				sw, err := s.GetSwarm(ctx, "s")
				visits, visitsErr := s.SwarmSteps(ctx, "s")
				if err != nil || visitsErr != nil || sw.Execution != "failed" || len(visits) != int(sequence)+1 || visits[sequence-1].Answer == nil || *visits[sequence-1].Answer != "fail" || visits[sequence].NodeID != "done" || visits[sequence].State != "completed" {
					t.Fatalf("rejected: swarm=%+v visits=%+v errors=%v, %v", sw, visits, err, visitsErr)
				}
				return
			}
			for _, node := range after {
				complete(node)
			}
			sw, err = s.GetSwarm(ctx, "s")
			visits, visitsErr = s.SwarmSteps(ctx, "s")
			if err != nil || visitsErr != nil || sw.Execution != "succeeded" || len(visits) != int(sequence)+1 || visits[sequence].NodeID != "done" || visits[sequence].State != "completed" {
				t.Fatalf("approved: swarm=%+v visits=%+v errors=%v, %v", sw, visits, err, visitsErr)
			}
		})
	}
}
