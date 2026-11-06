package build

// FEATURE: Named declarative workflows in Broodfile.
// SPEC: Validate pipeline graph structure and template references while preserving build plans.
// IMPLEMENTATION: Reuse pipeline validation and resolve templates in the Broodfile.
// RESULT: IMPLEMENTED — workflows are declarative and do not change build plans.
// STATUS: DONE

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// BUG: A valid small Broodfile can expand beyond the pipeline input size limit.
// HYPOTHESIS: Validation applies the input limit again to generated YAML.
// SOLUTION: Strictly parse generated workflow YAML without reapplying the input limit.
// RESULT: FIXED — the original Broodfile remains size-limited.
// STATUS: FIXED
func TestWorkflowExpandedYAML(t *testing.T) {
	doc := minimal(t)
	doc["workflows"] = map[string]any{"check": map[string]any{
		"start": "work", "exit": "done",
		"nodes": map[string]any{
			"work": map[string]any{"type": "actor", "prompt": strings.Repeat("x\n", 110000)},
			"done": map[string]any{"type": "exit"},
		},
		"edges": []any{map[string]any{"from": "work", "to": "done"}}, "goalGates": []any{"done"},
	}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 1<<20 {
		t.Fatalf("input is too large: %d bytes", len(data))
	}
	parsed, err := ParseDocument(string(data), "Broodfile")
	if err != nil {
		t.Fatalf("valid %d-byte Broodfile rejected: %v", len(data), err)
	}
	if _, err := MakePlan(parsed, nil, "", "localhost:5000/team", "development", "Broodfile"); err != nil {
		t.Fatalf("planning failed: %v", err)
	}
}

// FEATURE: Safe example workflow with a human decision.
// SPEC: An offline actor leads to two explicitly routed human options and one exit.
// IMPLEMENTATION: Keep the read-only example compatible with the limited runner.
// RESULT: IMPLEMENTED — the example includes a human decision.
// STATUS: DONE
func TestWorkflowExample(t *testing.T) {
	data, err := os.ReadFile("../../../examples/Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument(string(data), "examples/Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MakePlan(doc, nil, "", "localhost:5000/team", "development", "examples/Broodfile"); err != nil {
		t.Fatal(err)
	}
	graph, err := Workflow(doc, "inspect", "examples/Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	if graph.Start != "inspect" || graph.Exit != "done" || len(graph.Nodes) != 3 || graph.Nodes["inspect"].Type != "actor" || graph.Nodes["review"].Type != "human" || graph.Nodes["done"].Type != "exit" {
		t.Fatalf("unexpected graph: %+v", graph)
	}
	options := graph.Nodes["review"].Options
	if len(options) != 2 || options[0].Outcome != "success" || options[1].Outcome != "fail" || options[0].Label == "" || options[1].Label == "" {
		t.Fatalf("options: %+v", options)
	}
	if len(graph.Edges) != 3 {
		t.Fatalf("edges: %+v", graph.Edges)
	}
	for _, option := range options {
		found := false
		for _, edge := range graph.Edges {
			if edge.From == "review" && edge.To == "done" && edge.Label == option.Label {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing route for %+v", option)
		}
	}
}

func TestProjectAndWorkflow(t *testing.T) {
	doc := minimal(t)
	doc["project"] = map[string]any{"repository": "git@git.example.com:team/repo.git", "description": "Example"}
	doc["workflows"] = map[string]any{"check": map[string]any{
		"start": "work", "exit": "done",
		"nodes": map[string]any{"work": map[string]any{"type": "actor", "template": "python"}, "done": map[string]any{"type": "exit"}},
		"edges": []any{map[string]any{"from": "work", "to": "done"}}, "goalGates": []any{"done"},
	}}
	parsed, err := ParseDocument(marshal(t, doc), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := MakePlan(parsed, nil, "", "localhost:5000/team", "development", "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	config := plan["projectConfig"].(map[string]any)
	if config["repository"] != "https://git.example.com/team/repo.git" || config["description"] != "Example" {
		t.Fatalf("project config: %v", config)
	}
	graph, err := Workflow(parsed, "check", "Broodfile")
	if err != nil || graph.Nodes["work"].Template != "python" {
		t.Fatalf("workflow: %v, %v", graph, err)
	}
	parsed["workflows"].(map[string]any)["check"].(map[string]any)["exit"] = "missing"
	if _, err := MakePlan(parsed, nil, "", "localhost:5000/team", "development", "Broodfile"); err == nil {
		t.Fatal("invalid workflow accepted by MakePlan")
	}
}

func TestWorkflows(t *testing.T) {
	graph := map[string]any{
		"start": "work", "exit": "done",
		"nodes": map[string]any{"work": map[string]any{"type": "actor", "template": "python"}, "done": map[string]any{"type": "exit"}},
		"edges": []any{map[string]any{"from": "work", "to": "done"}}, "goalGates": []any{"done"},
	}
	t.Run("valid and build unchanged", func(t *testing.T) {
		before := minimal(t)
		after := deepCopy(t, before).(map[string]any)
		after["workflows"] = map[string]any{"check": graph}
		parsed, err := ParseDocument(marshal(t, after), "Broodfile")
		if err != nil {
			t.Fatal(err)
		}
		a, err := MakePlan(before, nil, "", "localhost:5000/team", "development", "Broodfile")
		if err != nil {
			t.Fatal(err)
		}
		b, err := MakePlan(parsed, nil, "", "localhost:5000/team", "development", "Broodfile")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatal("build plan changed")
		}
	})
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"unknown field", func(g map[string]any) { g["surprise"] = true }, "additionalProperties"},
		{"missing template", func(g map[string]any) { g["nodes"].(map[string]any)["work"].(map[string]any)["template"] = "missing" }, "missing"},
		{"missing endpoint", func(g map[string]any) { g["exit"] = "missing" }, "missing"},
		{"empty nodes", func(g map[string]any) { g["nodes"] = map[string]any{} }, "nodes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := minimal(t)
			g := deepCopy(t, graph).(map[string]any)
			tc.change(g)
			d["workflows"] = map[string]any{"check": g}
			_, err := ParseDocument(marshal(t, d), "Broodfile")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}
