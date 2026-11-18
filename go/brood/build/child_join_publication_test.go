package build

import (
	"testing"

	"brood.dev/go/brood/pipeline"
)

func childJoinPublicationDocument(t *testing.T, node string, policy map[string]any) map[string]any {
	t.Helper()
	document := minimal(t)
	nodes := map[string]any{
		"work": map[string]any{"type": "actor", "role": "build"},
		"done": map[string]any{"type": "exit"},
	}
	nodes[node].(map[string]any)["childJoin"] = policy
	document["workflows"] = map[string]any{"children": map[string]any{
		"start": "work", "exit": "done", "nodes": nodes,
		"edges": []any{map[string]any{"from": "work", "to": "done"}}, "goalGates": []any{"done"},
	}}
	return document
}

func TestBroodfileChildJoinSurvivesWorkflowPublication(t *testing.T) {
	for _, mode := range []string{"all", "any", "quorum"} {
		t.Run(mode, func(t *testing.T) {
			policy := map[string]any{"mode": mode}
			quorum := 0
			if mode == "quorum" {
				quorum = 2
				policy["quorum"] = quorum
			}
			document := childJoinPublicationDocument(t, "work", policy)
			parsed, err := ParseDocument(marshal(t, document), "Broodfile")
			if err != nil {
				t.Fatalf("valid node-owned %s join rejected by Broodfile: %v", mode, err)
			}
			snapshots, err := WorkflowSnapshots(parsed, "Broodfile")
			if err != nil {
				t.Fatal(err)
			}
			snapshot, exists := snapshots["children"]
			if !exists {
				t.Fatal("workflow omitted from publication")
			}
			graph, err := pipeline.Parse(snapshot, "published.yaml")
			if err != nil {
				t.Fatalf("invalid published graph: %v", err)
			}
			got := graph.Nodes["work"].ChildJoin
			if got == nil || got.Mode != mode || got.Quorum != quorum {
				t.Fatalf("publication changed node policy: got %+v, want mode=%s quorum=%d", got, mode, quorum)
			}
		})
	}
}

func TestBroodfileChildJoinRejectsInvalidPolicies(t *testing.T) {
	for _, tc := range []struct {
		name, node string
		policy     map[string]any
	}{
		{"unknown-mode", "work", map[string]any{"mode": "other"}},
		{"quorum-required", "work", map[string]any{"mode": "quorum"}},
		{"zero-quorum", "work", map[string]any{"mode": "quorum", "quorum": 0}},
		{"too-many", "work", map[string]any{"mode": "quorum", "quorum": 65}},
		{"any-with-quorum", "work", map[string]any{"mode": "any", "quorum": 2}},
		{"implicit-all-with-quorum", "work", map[string]any{"quorum": 2}},
		{"unknown-field", "work", map[string]any{"mode": "all", "extra": 1}},
		{"non-actor", "done", map[string]any{"mode": "all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := childJoinPublicationDocument(t, tc.node, tc.policy)
			if _, err := ParseDocument(marshal(t, document), "Broodfile"); err == nil {
				t.Fatal("invalid child join accepted")
			}
		})
	}
}
