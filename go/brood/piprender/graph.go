// Package piprender renders pipeline graphs for the brood web panel:
// deterministic DOT text and standalone SVG without external tools.
// Input types are local to this package; the pipeline package adapts them
// in a later issue. Rendering is pure: no graphviz binary, no network.
package piprender

// Kind is the structural role of a node in a pipeline graph.
type Kind string

// Node kinds of a pipeline. The mapping to DOT shapes lives in dot.go,
// to SVG marker styles in svg.go.
const (
	KindStart     Kind = "start"
	KindExit      Kind = "exit"
	KindActor     Kind = "actor"
	KindHuman     Kind = "human"
	KindCondition Kind = "condition"
	KindLand      Kind = "land"
	KindParallel  Kind = "parallel"
	KindFanIn     Kind = "fan_in"
)

// Node is one step of a pipeline.
type Node struct {
	ID    string
	Kind  Kind
	Label string
	// Order overrides the automatic by-ID sort of DOT output; nodes with
	// equal Order keep the ID comparison. Zero means no override.
	Order int
}

// Edge is a directed transition between two nodes.
type Edge struct {
	From string
	To   string
	// Label is the option name, for example a branch of a condition.
	Label string
	// Condition is the guard text; on the wire it wins over Label.
	Condition string
	// Weight, when nonzero, is appended to the edge label as ` w=N`.
	Weight int
}

// Graph is a pipeline: nodes and directed edges between them.
type Graph struct {
	Nodes []Node
	Edges []Edge
}
