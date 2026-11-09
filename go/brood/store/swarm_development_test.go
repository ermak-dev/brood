package store

import (
	"context"
	"fmt"
	"os"
	"testing"

	"brood.dev/go/brood/build"
	"gopkg.in/yaml.v3"
)

// TestRootSwarmWorkflowProgression exercises applying engine transitions of the
// repository's architecture graph through the store: both decision answers
// land in the right visit and status. Pure route rules (plain chain, fail
// edges) are engine tests in the pipeline package (#207); legacy runs here
// deliberately have no source snapshot — source-bound execution stays fenced.
func TestRootSwarmWorkflowProgression(t *testing.T) {
	data, err := os.ReadFile("../../../Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := build.ParseDocument(string(data), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	graph, err := build.Workflow(doc, "swarm-architecture", "Broodfile")
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
			if err := createLegacyPipelineRun(ctx, s, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: snapshot, Status: "running"}); err != nil {
				t.Fatal(err)
			}
			sequence := int64(0)
			complete := func(node string) {
				t.Helper()
				sequence++
				request := fmt.Sprintf("request-%d", sequence)
				if _, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: node, State: "inflight", ActorUID: "actor", ActorIncarnation: 1, RequestID: request}); err != nil {
					t.Fatal(err)
				}
				if err := s.CompletePipelineActorVisit(ctx, "s", "r", sequence, request, "actor", 1, "success"); err != nil {
					t.Fatal(err)
				}
			}
			for _, node := range before {
				complete(node)
			}
			run, err := s.GetPipelineRun(ctx, "s", "r")
			visits, visitsErr := s.ListPipelineVisits(ctx, "s", "r")
			if err != nil || visitsErr != nil || run.Status != "waiting_human" || len(visits) != int(sequence)+1 || visits[sequence].NodeID != "decision" || visits[sequence].State != "waiting" {
				t.Fatalf("gated run: run=%+v visits=%+v errors=%v, %v", run, visits, err, visitsErr)
			}
			sequence++
			if err := s.AnswerPipelineVisit(ctx, "s", "r", sequence, tc.answer); err != nil {
				t.Fatal(err)
			}
			if tc.answer == "fail" {
				run, err := s.GetPipelineRun(ctx, "s", "r")
				visits, visitsErr := s.ListPipelineVisits(ctx, "s", "r")
				if err != nil || visitsErr != nil || run.Status != "failed" || len(visits) != int(sequence)+1 || visits[sequence-1].Answer == nil || *visits[sequence-1].Answer != "fail" || visits[sequence].NodeID != "done" || visits[sequence].State != "completed" {
					t.Fatalf("rejected: run=%+v visits=%+v errors=%v, %v", run, visits, err, visitsErr)
				}
				return
			}
			for _, node := range after {
				complete(node)
			}
			run, err = s.GetPipelineRun(ctx, "s", "r")
			visits, visitsErr = s.ListPipelineVisits(ctx, "s", "r")
			if err != nil || visitsErr != nil || run.Status != "succeeded" || len(visits) != int(sequence)+1 || visits[sequence].NodeID != "done" || visits[sequence].State != "completed" {
				t.Fatalf("approved: run=%+v visits=%+v errors=%v, %v", run, visits, err, visitsErr)
			}
		})
	}
}
