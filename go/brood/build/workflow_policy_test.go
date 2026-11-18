package build

import (
	"reflect"
	"testing"

	"brood.dev/go/brood/pipeline"
)

func TestWorkflowActionClasses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		classes any
		valid   bool
	}{
		{"protected", []any{"schema", "deploy"}, true},
		{"ordinary", []any{"work"}, true},
		{"empty", []any{}, false},
		{"unknown", []any{"unknown"}, false},
		{"duplicate", []any{"landing", "landing"}, false},
		{"mixed work", []any{"work", "landing"}, false},
		{"scalar", "schema", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := minimal(t)
			doc["workflows"] = map[string]any{"check": map[string]any{
				"start": "work", "exit": "done",
				"nodes": map[string]any{"work": map[string]any{"type": "actor", "role": "build", "actionClasses": tc.classes, "childJoin": map[string]any{"mode": "quorum", "quorum": 2}}, "done": map[string]any{"type": "exit"}},
				"edges": []any{map[string]any{"from": "work", "to": "done"}}, "goalGates": []any{"done"},
			}}
			parsed, err := ParseDocument(marshal(t, doc), "Broodfile")
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid classification accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshots, err := WorkflowSnapshots(parsed, "Broodfile")
			if err != nil {
				t.Fatal(err)
			}
			graph, err := pipeline.Parse(snapshots["check"], "published.yaml")
			if err != nil {
				t.Fatal(err)
			}
			join := graph.Nodes["work"].ChildJoin
			if join == nil || join.Mode != "quorum" || join.Quorum != 2 {
				t.Fatal("action publication changed child join policy")
			}
			var want []string
			for _, value := range tc.classes.([]any) {
				want = append(want, value.(string))
			}
			if !reflect.DeepEqual(graph.Nodes["work"].ActionClasses, want) {
				t.Fatal("workflow publication lost action classes")
			}
		})
	}
}
