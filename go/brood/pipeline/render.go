package pipeline

import "brood.dev/go/brood/piprender"

// Graph adapts a document to the shared graph renderer without exposing node contents.
func (d *Document) Graph() piprender.Graph {
	var g piprender.Graph
	if d == nil {
		return g
	}
	for _, id := range d.sortedNodeNames() {
		kind := piprender.Kind(d.Nodes[id].Type)
		if id == d.Exit {
			kind = piprender.KindExit
		} else if id == d.Start {
			kind = piprender.KindStart
		}
		g.Nodes = append(g.Nodes, piprender.Node{ID: id, Label: id, Kind: kind})
	}
	for _, e := range d.Edges {
		g.Edges = append(g.Edges, piprender.Edge{From: e.From, To: e.To, Label: e.Label, Condition: e.Condition, Weight: e.Weight})
	}
	return g
}

// RenderDot validates and renders the document as Graphviz DOT.
func (d *Document) RenderDot() (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	return piprender.DOT(d.Graph()), nil
}
