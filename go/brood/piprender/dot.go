package piprender

import (
	"fmt"
	"sort"
	"strings"
)

// dotShapes maps node kinds to graphviz polygon shapes.
var dotShapes = map[Kind]string{
	KindStart:     "Mdiamond",
	KindExit:      "Msquare",
	KindActor:     "box",
	KindHuman:     "hexagon",
	KindCondition: "diamond",
	KindLand:      "house",
	KindParallel:  "component",
	KindFanIn:     "tripleoctagon",
}

// quoteID renders a DOT identifier, quoting only when the plain form is
// not alphanumeric-with-underscore as DOT requires.
func quoteID(id string) string {
	if id == "" {
		return `""`
	}
	switch strings.ToLower(id) {
	case "strict", "graph", "digraph", "node", "edge", "subgraph":
		return `"` + id + `"`
	}
	for i, r := range id {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(id) + `"`
	}
	return id
}

// quoteLabel renders a DOT attribute string with XML-style escaping and
// double quotes.
func quoteLabel(s string) string {
	escaped := strings.NewReplacer(`"`, `\"`, `\`, `\\`, "\n", `\n`, "\r", `\r`).Replace(s)
	return `"` + escaped + `"`
}

// sortedNodes returns the nodes in the deterministic DOT order: explicit
// Order first ascending, then ID ascending. The input is not modified.
func sortedNodes(nodes []Node) []Node {
	out := make([]Node, len(nodes))
	copy(out, nodes)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// sortedEdges returns the edges sorted by (From, To). Parallel edges keep
// their relative input order. The input is not modified.
func sortedEdges(edges []Edge) []Edge {
	out := make([]Edge, len(edges))
	copy(out, edges)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// edgeLabel joins the edge caption: condition text or option label, with
// ` w=N` appended for a nonzero weight.
func edgeLabel(e Edge) string {
	caption := e.Condition
	if caption == "" {
		caption = e.Label
	}
	if e.Weight != 0 {
		if caption == "" {
			return fmt.Sprintf("w=%d", e.Weight)
		}
		return fmt.Sprintf("%s w=%d", caption, e.Weight)
	}
	return caption
}

// DOT renders the graph as graphviz DOT. Output is deterministic:
// nodes follow Order then ID, edges follow (From, To), and attribute
// order is fixed.
func DOT(g Graph) string {
	var b strings.Builder
	b.WriteString("digraph pipeline {\n")
	b.WriteString("  rankdir=LR;\n")
	for _, n := range sortedNodes(g.Nodes) {
		shape, ok := dotShapes[n.Kind]
		if !ok {
			shape = "box"
		}
		fmt.Fprintf(&b, "  %s [label=%s, shape=%s];\n", quoteID(n.ID), quoteLabel(n.Label), shape)
	}
	for _, e := range sortedEdges(g.Edges) {
		b.WriteString("  ")
		b.WriteString(quoteID(e.From))
		b.WriteString(" -> ")
		b.WriteString(quoteID(e.To))
		if label := edgeLabel(e); label != "" {
			b.WriteString(" [label=")
			b.WriteString(quoteLabel(label))
			b.WriteString("]")
		}
		b.WriteString(";\n")
	}
	b.WriteString("}\n")
	return b.String()
}
