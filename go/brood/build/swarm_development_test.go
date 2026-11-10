package build

// FEATURE: Два определения разработки роем в корневом Broodfile.
// SPEC: Обычный путь без человека; архитектурный путь пропускает только одобрение.
// IMPLEMENTATION: Два графа с явной маршрутизацией успешных исходов и отказа;
// узлы называют роли. Число ролей графа сверяется с принятой константой
// maxWorkflowRoles, а не с ёмкостью пула (#221).
// RESULT: IMPLEMENTED — определения проходят общую проверку и проверку маршрутов.
// STATUS: DONE

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"brood.dev/go/brood/pipeline"
)

// maxWorkflowRoles — принятая граница числа различных ролей в одном
// опубликованном графе корневого Broodfile. Это не maxActors пула и не
// гарантия ёмкости: пул проекта в Broodfile не объявлен, а несколько ролей
// вправе делиться одним актором. Сверки ёмкости пула с привязками ролей пока
// нет нигде, её добавит запуск роя (#221); тест лишь ловит рост числа ролей
// в Broodfile.
const maxWorkflowRoles = 3

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
				} else if node.Type != "actor" || node.Role == "" || node.Prompt == "" {
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

// TestSwarmWorkflowRoleCountBounded — опубликованный граф называет не более
// maxWorkflowRoles ролей. Ёмкость пула и число акторов тест не проверяет (#221).
func TestSwarmWorkflowRoleCountBounded(t *testing.T) {
	data, err := os.ReadFile("../../../Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument(string(data), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	workflows, _ := doc["workflows"].(map[string]any)
	if len(workflows) == 0 {
		t.Fatal("no published workflows")
	}
	for name := range workflows {
		graph, err := Workflow(doc, name, "Broodfile")
		if err != nil {
			t.Fatal(err)
		}
		roles := graph.Roles()
		if len(roles) > maxWorkflowRoles {
			t.Fatalf("workflow %s names %d roles, expected at most %d", name, len(roles), maxWorkflowRoles)
		}
	}
}

// TestSwarmWorkflowRoleSplit — обзоры и реализация названы разными ролями.
// Разные ли акторы их займут, решает привязка роя: один актор вправе держать
// обе роли, и тогда независимость обзора граф не обеспечивает.
func TestSwarmWorkflowRoleSplit(t *testing.T) {
	data, err := os.ReadFile("../../../Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument(string(data), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{
		"swarm-development": {
			"triage": "review", "review": "review",
			"implement": "build", "test": "build", "integrate": "build",
		},
		"swarm-architecture": {
			"triage": "review", "architecture": "review", "review": "review",
			"implement": "build", "test": "build", "integrate": "build",
		},
	}
	for workflow, nodes := range want {
		graph, err := Workflow(doc, workflow, "Broodfile")
		if err != nil {
			t.Fatal(err)
		}
		for node, role := range nodes {
			if graph.Nodes[node].Role != role {
				t.Fatalf("%s.%s: role %q, want %q", workflow, node, graph.Nodes[node].Role, role)
			}
		}
		if len(graph.Roles()) != 2 {
			t.Fatalf("%s: expected exactly two roles, got %v", workflow, graph.Roles())
		}
	}
}
