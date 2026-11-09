package store

import (
	"context"
	"fmt"
	"os"
	"testing"

	"brood.dev/go/brood/build"
	"gopkg.in/yaml.v3"
)

// TestRootSwarmWorkflowProgression exercises the repository's graphs through the store.
// Legacy runs deliberately have no source snapshot; source-bound execution remains fenced.
func TestRootSwarmWorkflowProgression(t *testing.T) {
	data, err := os.ReadFile("../../../Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := build.ParseDocument(string(data), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, workflow := range []struct {
		name          string
		before, after []string
	}{
		{"swarm-development", []string{"triage"}, []string{"implement", "review", "test", "integrate"}},
		{"swarm-architecture", []string{"triage", "architecture"}, []string{"implement", "review", "test", "integrate"}},
	} {
		t.Run(workflow.name, func(t *testing.T) {
			graph, err := build.Workflow(doc, workflow.name, "Broodfile")
			if err != nil {
				t.Fatal(err)
			}
			if workflow.name == "swarm-architecture" {
				options := make(map[string]string)
				for _, option := range graph.Nodes["decision"].Options {
					options[option.Label] = option.Outcome
				}
				if len(options) != 2 || options["Approve"] != "success" || options["Reject"] != "fail" {
					t.Fatalf("decision options: %+v", options)
				}
			}
			snapshot, err := yaml.Marshal(graph)
			if err != nil {
				t.Fatal(err)
			}
			actors := append(append([]string{}, workflow.before...), workflow.after...)
			for _, failNode := range append([]string{""}, actors...) {
				name := "success"
				if failNode != "" {
					name = "fail-" + failNode
				}
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					s := sourceTestStore(t)
					if err := createLegacyPipelineRun(ctx, s, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: snapshot, Status: "running"}); err != nil {
						t.Fatal(err)
					}
					sequence := int64(0)
					check := func(status, node, state string) {
						t.Helper()
						run, err := s.GetPipelineRun(ctx, "s", "r")
						if err != nil || run.Status != status {
							t.Fatalf("status: got %+v, want %s: %v", run, status, err)
						}
						visits, err := s.ListPipelineVisits(ctx, "s", "r")
						if err != nil || len(visits) != int(sequence)+1 || visits[len(visits)-1].NodeID != node || visits[len(visits)-1].State != state {
							t.Fatalf("next visit: got %+v, want %s/%s after %d: %v", visits, node, state, sequence, err)
						}
					}
					complete := func(node string) bool {
						t.Helper()
						sequence++
						request := fmt.Sprintf("request-%d", sequence)
						if _, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: node, State: "inflight", ActorUID: "actor", ActorIncarnation: 1, RequestID: request}); err != nil {
							t.Fatal(err)
						}
						outcome := "success"
						if node == failNode {
							outcome = "fail"
						}
						if err := s.CompletePipelineActorVisit(ctx, "s", "r", sequence, request, "actor", 1, outcome); err != nil {
							t.Fatal(err)
						}
						if outcome == "fail" {
							run, err := s.GetPipelineRun(ctx, "s", "r")
							visits, visitsErr := s.ListPipelineVisits(ctx, "s", "r")
							if err != nil || visitsErr != nil || run.Status != "failed" || len(visits) != int(sequence) {
								t.Fatalf("failed actor: run=%+v visits=%+v errors=%v, %v", run, visits, err, visitsErr)
							}
							return false
						}
						return true
					}
					for _, node := range workflow.before {
						if !complete(node) {
							return
						}
					}
					if workflow.name == "swarm-architecture" {
						check("waiting_human", "decision", "waiting")
						sequence++
						if err := s.AnswerPipelineVisit(ctx, "s", "r", sequence, "success"); err != nil {
							t.Fatal(err)
						}
						check("running", "implement", "pending")
					}
					for _, node := range workflow.after {
						if !complete(node) {
							return
						}
					}
					check("succeeded", "done", "completed")
				})
			}
			if workflow.name == "swarm-architecture" {
				t.Run("reject", func(t *testing.T) {
					ctx := context.Background()
					s := sourceTestStore(t)
					if err := createLegacyPipelineRun(ctx, s, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: snapshot, Status: "running"}); err != nil {
						t.Fatal(err)
					}
					for i, node := range workflow.before {
						request := fmt.Sprintf("request-%d", i+1)
						if _, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: node, State: "inflight", ActorUID: "actor", ActorIncarnation: 1, RequestID: request}); err != nil {
							t.Fatal(err)
						}
						if err := s.CompletePipelineActorVisit(ctx, "s", "r", int64(i+1), request, "actor", 1, "success"); err != nil {
							t.Fatal(err)
						}
					}
					if err := s.AnswerPipelineVisit(ctx, "s", "r", 3, "fail"); err != nil {
						t.Fatal(err)
					}
					run, err := s.GetPipelineRun(ctx, "s", "r")
					visits, visitsErr := s.ListPipelineVisits(ctx, "s", "r")
					if err != nil || visitsErr != nil || run.Status != "failed" || len(visits) != 4 || visits[2].NodeID != "decision" || visits[2].Answer == nil || *visits[2].Answer != "fail" || visits[3].NodeID != "done" {
						t.Fatalf("rejected: run=%+v visits=%+v errors=%v, %v", run, visits, err, visitsErr)
					}
				})
			}
		})
	}
}
