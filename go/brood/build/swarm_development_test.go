package build

// FEATURE: Два определения разработки роем в корневом Broodfile.
// SPEC: Обычный путь без человека; архитектурный путь пропускает только одобрение.
// IMPLEMENTATION: Два графа с явной маршрутизацией успешных исходов и отказа.
// RESULT: IMPLEMENTED — определения проходят общую проверку и проверку маршрутов.
// STATUS: DONE

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"brood.dev/go/brood/pipeline"
)

func TestSwarmDevelopmentWorkflows(t *testing.T) {
	data, err := os.ReadFile("../../../Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument(string(data), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	pinData, err := os.ReadFile("../../../brood/dev/pins.json")
	if err != nil {
		t.Fatal(err)
	}
	var pins map[string]string
	if err := json.Unmarshal(pinData, &pins); err != nil {
		t.Fatal(err)
	}
	if _, err := MakePlan(doc, pins, "", "localhost:5000/team", "development", "Broodfile"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		stages []string
		human  bool
	}{
		{"swarm-development", []string{"triage", "implement", "review", "test", "integrate", "done"}, false},
		{"swarm-architecture", []string{"triage", "architecture", "decision", "implement", "review", "test", "integrate", "done"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph, err := Workflow(doc, tc.name, "Broodfile")
			if err != nil {
				t.Fatal(err)
			}
			if graph.Start != tc.stages[0] || graph.Exit != "done" || !reflect.DeepEqual(graph.GoalGates, []string{"done"}) {
				t.Fatalf("unexpected endpoints or gates: %+v", graph)
			}
			if graph.Retry != nil && graph.Retry.MaxRetries != 0 {
				t.Fatal("automatic retry is unsupported")
			}
			if len(graph.Nodes) != len(tc.stages) {
				t.Fatalf("unexpected nodes: %+v", graph.Nodes)
			}
			for i, name := range tc.stages {
				node := graph.Nodes[name]
				if name == "done" {
					if node.Type != "exit" {
						t.Fatalf("%s is not exit", name)
					}
					continue
				}
				if name == "decision" {
					if !tc.human || node.Type != "human" || len(node.Options) != 2 {
						t.Fatalf("invalid decision: %+v", node)
					}
					if node.Options[0].Outcome != "success" || node.Options[1].Outcome != "fail" {
						t.Fatalf("decision outcomes: %+v", node.Options)
					}
				} else if node.Type != "actor" || node.Template != "coding" || node.Prompt == "" {
					t.Fatalf("invalid actor %s: %+v", name, node)
				}
				var outgoing []pipeline.Edge
				for _, edge := range graph.Edges {
					if edge.From == name {
						outgoing = append(outgoing, edge)
					}
				}
				edge, ok := pipeline.SelectNext(outgoing, "success", nil, "", nil)
				if !ok || edge.To != tc.stages[i+1] || edge.Condition != "outcome=success" {
					t.Fatalf("%s success routing: %+v", name, edge)
				}
				edge, ok = pipeline.SelectNext(outgoing, "fail", nil, "", nil)
				if name == "decision" {
					if !ok || edge.To != "done" || edge.Condition != "outcome=fail" {
						t.Fatalf("reject routing: %+v", edge)
					}
				} else if ok {
					t.Fatalf("%s failure must stop, got %+v", name, edge)
				}
			}
		})
	}
}
