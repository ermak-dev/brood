package piprender

import (
	"reflect"
	"testing"
)

// chain — линейный граф a -> b -> c -> d с label, равным ID.
func chain() Graph {
	return Graph{
		Nodes: []Node{
			{ID: "a", Kind: KindStart, Label: "a"},
			{ID: "b", Kind: KindActor, Label: "b"},
			{ID: "c", Kind: KindActor, Label: "c"},
			{ID: "d", Kind: KindExit, Label: "d"},
		},
		Edges: []Edge{
			{From: "a", To: "b"},
			{From: "b", To: "c"},
			{From: "c", To: "d"},
		},
	}
}

func TestOverlayStates(t *testing.T) {
	fresh := Overlay(chain(), nil, "", nil)
	if !reflect.DeepEqual(fresh.Nodes, map[string]State{
		"a": StatePending, "b": StatePending, "c": StatePending, "d": StatePending,
	}) {
		t.Fatalf("fresh run nodes: %v", fresh.Nodes)
	}
	for i, st := range fresh.Edges {
		if st != StatePending {
			t.Fatalf("fresh run edge %d: %s", i, st)
		}
	}

	running := Overlay(chain(), []string{"a", "b"}, "c", nil)
	if !reflect.DeepEqual(running.Nodes, map[string]State{
		"a": StateDone, "b": StateDone, "c": StateActive, "d": StatePending,
	}) {
		t.Fatalf("running nodes: %v", running.Nodes)
	}
	if !reflect.DeepEqual(running.Edges, map[int]State{
		0: StateDone, 1: StateActive, 2: StatePending,
	}) {
		t.Fatalf("running edges: %v", running.Edges)
	}

	done := Overlay(chain(), []string{"a", "b", "c", "d"}, "", nil)
	for id, st := range done.Nodes {
		if st != StateDone {
			t.Fatalf("finished node %s: %s", id, st)
		}
	}
	for i, st := range done.Edges {
		if st != StateDone {
			t.Fatalf("finished edge %d: %s", i, st)
		}
	}

	// Провал на b: ребро a->b pending (обе стороны не failed-источник),
	// из failed b исходящие failed, после него узлы skipped.
	failed := Overlay(chain(), []string{"a"}, "", []string{"b"})
	if !reflect.DeepEqual(failed.Nodes, map[string]State{
		"a": StateDone, "b": StateFailed, "c": StateSkipped, "d": StateSkipped,
	}) {
		t.Fatalf("failed run nodes: %v", failed.Nodes)
	}
	if !reflect.DeepEqual(failed.Edges, map[int]State{
		0: StatePending, 1: StateFailed, 2: StatePending,
	}) {
		t.Fatalf("failed run edges: %v", failed.Edges)
	}

	// Провал при текущем шаге: текущий не одновременно active и failed.
	both := Overlay(chain(), []string{"a"}, "b", []string{"b"})
	if both.Nodes["b"] != StateFailed {
		t.Fatalf("failed current must be failed: %v", both.Nodes)
	}
}

func TestOverlaySkippedNeedsAllPredecessorsFailed(t *testing.T) {
	g := Graph{
		Nodes: []Node{
			{ID: "s", Kind: KindStart},
			{ID: "a", Kind: KindActor},
			{ID: "b", Kind: KindActor},
			{ID: "join", Kind: KindFanIn},
		},
		Edges: []Edge{
			{From: "s", To: "a"},
			{From: "s", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
		},
	}
	// Провалена только ветка a: join ждёт живую ветку b, не skipped.
	v := Overlay(g, []string{"s"}, "", []string{"a"})
	if v.Nodes["join"] != StatePending {
		t.Fatalf("join must stay pending while branch b is alive: %v", v.Nodes)
	}
	if v.Edges[2] != StateFailed {
		t.Fatalf("edge a->join must be failed: %v", v.Edges)
	}
	// Провалены обе ветки: join skipped.
	v = Overlay(g, []string{"s"}, "", []string{"a", "b"})
	if v.Nodes["join"] != StateSkipped {
		t.Fatalf("join must be skipped when all branches failed: %v", v.Nodes)
	}
}

func TestOverlayUnknownIDsIgnored(t *testing.T) {
	v := Overlay(chain(), []string{"a", "ghost"}, "b", []string{"phantom"})
	if _, ok := v.Nodes["ghost"]; ok {
		t.Fatal("unknown completed ID leaked into node states")
	}
	if v.Nodes["c"] != StatePending {
		t.Fatalf("unknown failed ID changed state of c: %v", v.Nodes)
	}
}

func TestOverlayOverlay(t *testing.T) {
	// Overlay не меняет граф: RunView.Graph пригоден для повторной
	// разметки тем же набором.
	g := chain()
	v1 := Overlay(g, []string{"a"}, "b", nil)
	v2 := Overlay(v1.Graph, []string{"a"}, "b", nil)
	if !reflect.DeepEqual(v1, v2) {
		t.Fatal("graph changed by overlay")
	}
}
