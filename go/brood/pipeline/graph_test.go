package pipeline

import (
	"reflect"
	"testing"
)

// FEATURE: Linear graph reachability.
// SPEC: Forward and reverse walks preserve reachability for branches, cycles and undefined endpoints.
// IMPLEMENTATION: Index edges in both directions before iterative traversal.
// RESULT: IMPLEMENTED — traversal visits each reachable vertex and edge once.
// STATUS: DONE
func TestGraphReachability(t *testing.T) {
	cases := []struct {
		name             string
		edges            []Edge
		start            string
		forward, reverse map[string]bool
	}{
		{"empty", nil, "a", map[string]bool{"a": true}, map[string]bool{"a": true}},
		{"branch and cycle", []Edge{{From: "a", To: "b"}, {From: "a", To: "c"}, {From: "b", To: "a"}}, "a", map[string]bool{"a": true, "b": true, "c": true}, map[string]bool{"a": true, "b": true}},
		{"dangling", []Edge{{From: "a", To: "missing"}, {From: "missing", To: "b"}}, "b", map[string]bool{"b": true}, map[string]bool{"a": true, "missing": true, "b": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forward, reverse := graphAdjacency(tc.edges)
			if got := graphReachable(forward, tc.start); !reflect.DeepEqual(got, tc.forward) {
				t.Errorf("forward = %v, want %v", got, tc.forward)
			}
			if got := graphReachable(reverse, tc.start); !reflect.DeepEqual(got, tc.reverse) {
				t.Errorf("reverse = %v, want %v", got, tc.reverse)
			}
		})
	}
}

func TestGraphReachabilityEquivalence(t *testing.T) {
	// Сравниваем все направленные графы из трёх вершин, включая петли.
	names := []string{"a", "b", "missing"}
	for mask := 0; mask < 1<<9; mask++ {
		var edges []Edge
		for i, from := range names {
			for j, to := range names {
				if mask&(1<<(i*3+j)) != 0 {
					edges = append(edges, Edge{From: from, To: to})
				}
			}
		}
		forward, reverse := graphAdjacency(edges)
		for _, start := range names {
			for _, backwards := range []bool{false, true} {
				want := map[string]bool{start: true}
				for changed := true; changed; {
					changed = false
					for _, edge := range edges {
						from, to := edge.From, edge.To
						if backwards {
							from, to = to, from
						}
						if want[from] && !want[to] {
							want[to] = true
							changed = true
						}
					}
				}
				adj := forward
				if backwards {
					adj = reverse
				}
				if got := graphReachable(adj, start); !reflect.DeepEqual(got, want) {
					t.Fatalf("mask=%d start=%s reverse=%v got=%v want=%v", mask, start, backwards, got, want)
				}
			}
		}
	}
}
