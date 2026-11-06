package piprender

// State is the run state of a node or edge in a RunView.
type State string

// Run states used by the CSS of the web panel.
const (
	StatePending State = "pending"
	StateActive  State = "active"
	StateDone    State = "done"
	StateFailed  State = "failed"
	StateSkipped State = "skipped"
)

// RunView is a graph annotated with run states derived from a run log.
type RunView struct {
	Graph Graph
	// Nodes maps a node ID to its state; every node of Graph has an entry.
	Nodes map[string]State
	// Edges maps an edge index of Graph.Edges to its state.
	Edges map[int]State
}

// Overlay derives the run view of g: completed, current and failed are
// node ID sets (a set is a slice; duplicates are harmless). State rules:
//
//   - node: failed if in failed, active if == current, done if in
//     completed, skipped if pending and every predecessor is failed or
//     skipped (computed to a fixpoint), else pending
//   - edge: failed if its From is failed, done if both ends are done,
//     active if From is done and To == current, else pending
//
// Unknown IDs in the inputs are ignored.
func Overlay(g Graph, completed []string, current string, failed []string) RunView {
	doneSet := make(map[string]bool, len(completed))
	for _, id := range completed {
		doneSet[id] = true
	}
	failSet := make(map[string]bool, len(failed))
	for _, id := range failed {
		failSet[id] = true
	}

	nodes := make(map[string]State, len(g.Nodes))
	for _, n := range g.Nodes {
		switch {
		case failSet[n.ID]:
			nodes[n.ID] = StateFailed
		case n.ID == current:
			nodes[n.ID] = StateActive
		case doneSet[n.ID]:
			nodes[n.ID] = StateDone
		default:
			nodes[n.ID] = StatePending
		}
	}
	// Second pass, to a fixpoint: a pending node is skipped when it has
	// incoming edges and none of them can ever fire — every predecessor
	// is failed or itself skipped. Source nodes without incoming edges
	// stay pending: a not-yet-started run has no skipped steps.
	for progress := true; progress; {
		progress = false
		for _, n := range g.Nodes {
			if nodes[n.ID] != StatePending {
				continue
			}
			hasIncoming := false
			blocked := true
			for _, e := range g.Edges {
				if e.To != n.ID {
					continue
				}
				hasIncoming = true
				if !failSet[e.From] && nodes[e.From] != StateFailed && nodes[e.From] != StateSkipped {
					blocked = false
					break
				}
			}
			if hasIncoming && blocked {
				nodes[n.ID] = StateSkipped
				progress = true
			}
		}
	}

	edges := make(map[int]State, len(g.Edges))
	for i, e := range g.Edges {
		switch {
		case failSet[e.From] || nodes[e.From] == StateFailed:
			edges[i] = StateFailed
		case nodes[e.From] == StateDone && nodes[e.To] == StateDone:
			edges[i] = StateDone
		case nodes[e.From] == StateDone && e.To == current:
			edges[i] = StateActive
		default:
			edges[i] = StatePending
		}
	}
	return RunView{Graph: g, Nodes: nodes, Edges: edges}
}
