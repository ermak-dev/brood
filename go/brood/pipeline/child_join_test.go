package pipeline

import (
	"strings"
	"testing"
)

func TestActorNodeChildJoinPolicySchema(t *testing.T) {
	const graph = `apiVersion: brood.dev/v1alpha1
name: children
start: build
exit: done
nodes:
  build: {type: actor, role: build%s}
  done: {type: exit%s}
edges: [{from: build, to: done}]
goalGates: [done]
`
	cases := []struct {
		name, actor, exit string
		valid             bool
	}{
		{"default", "", "", true},
		{"all", ", childJoin: {mode: all}", "", true},
		{"any", ", childJoin: {mode: any}", "", true},
		{"quorum", ", childJoin: {mode: quorum, quorum: 2}", "", true},
		{"empty_defaults_all", ", childJoin: {}", "", true},
		{"unknown", ", childJoin: {mode: some}", "", false},
		{"missing_quorum", ", childJoin: {mode: quorum}", "", false},
		{"zero_quorum", ", childJoin: {mode: quorum, quorum: 0}", "", false},
		{"negative_quorum", ", childJoin: {mode: quorum, quorum: -1}", "", false},
		{"unbounded_quorum", ", childJoin: {mode: quorum, quorum: 65}", "", false},
		{"all_with_quorum", ", childJoin: {mode: all, quorum: 2}", "", false},
		{"any_with_quorum", ", childJoin: {mode: any, quorum: 2}", "", false},
		{"default_with_quorum", ", childJoin: {quorum: 2}", "", false},
		{"non_actor", "", ", childJoin: {mode: any}", false},
		{"unknown_field", ", childJoin: {mode: all, total: 2}", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Replace(graph, "%s", tc.actor, 1)
			text = strings.Replace(text, "%s", tc.exit, 1)
			d, err := Parse(text, "policy.yaml")
			if tc.valid {
				if err != nil {
					t.Fatal("node-defined join policy rejected", err)
				}
				if err := d.Validate(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid join policy accepted")
			}
		})
	}
}
