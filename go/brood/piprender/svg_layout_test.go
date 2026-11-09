package piprender

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// workflowGraph повторяет swarm-architecture из Broodfile: линейная
// цепочка и обходное ребро отказа decision -> done поверх четырёх строк.
func workflowGraph() Graph {
	kinds := map[string]Kind{"decision": KindHuman, "done": KindExit}
	ids := []string{"triage", "architecture", "decision", "implement", "review", "test", "integrate", "done"}
	g := Graph{}
	for _, id := range ids {
		kind := KindActor
		if k, ok := kinds[id]; ok {
			kind = k
		}
		g.Nodes = append(g.Nodes, Node{ID: id, Kind: kind, Label: id})
	}
	for _, e := range [][2]string{
		{"triage", "architecture"}, {"architecture", "decision"},
		{"decision", "implement"}, {"decision", "done"},
		{"implement", "review"}, {"review", "test"}, {"test", "integrate"},
		{"integrate", "done"},
	} {
		g.Edges = append(g.Edges, Edge{From: e[0], To: e[1], Condition: "outcome=success"})
	}
	g.Edges[3].Condition = "outcome=fail"
	return g
}

// cycleGraph — цикл a -> b -> c -> a с подписями рёбер.
func cycleGraph() Graph {
	return Graph{
		Nodes: []Node{
			{ID: "a", Kind: KindStart, Label: "a"},
			{ID: "b", Kind: KindActor, Label: "b"},
			{ID: "c", Kind: KindExit, Label: "c"},
		},
		Edges: []Edge{
			{From: "a", To: "b", Label: "next"},
			{From: "b", To: "c", Label: "next"},
			{From: "c", To: "a", Label: "retry"},
		},
	}
}

// yOfLabel и xOfLabel — общие хелперы svg_test.go.

func viewBoxOf(t *testing.T, svg string) [4]float64 {
	t.Helper()
	i := strings.Index(svg, `viewBox="`)
	end := strings.Index(svg[i+9:], `"`)
	parts := strings.Fields(svg[i+9 : i+9+end])
	if len(parts) != 4 {
		t.Fatalf("viewBox must have four numbers: %q", svg[i:i+60])
	}
	var nums [4]float64
	for j, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil {
			t.Fatalf("viewBox number %q: %v", p, err)
		}
		nums[j] = n
	}
	return nums
}

// FEATURE: Top-down graph orientation.
// SPEC: Layers are rows: a linear chain of eight nodes stacks vertically in one column.
// IMPLEMENTATION: Swap the layout axes in SVG and set rankdir=TB in DOT.
// RESULT: PENDING
// STATUS: RED
func TestSVGTopDown(t *testing.T) {
	g := workflowGraph()
	svg := SVG(RunView{Graph: g})
	ids := []string{"triage", "architecture", "decision", "implement", "review", "test", "integrate", "done"}
	ys := make(map[string]float64, len(ids))
	xs := make(map[string]float64, len(ids))
	for _, id := range ids {
		ys[id] = yOfLabel(t, svg, id)
		xs[id] = xOfLabel(t, svg, id)
	}
	for i := 1; i < len(ids); i++ {
		if !(ys[ids[i-1]] < ys[ids[i]]) {
			t.Fatalf("chain must go top down: %s y=%v not above %s y=%v\n%s", ids[i-1], ys[ids[i-1]], ids[i], ys[ids[i]], svg)
		}
		if xs[ids[i]] != xs[ids[0]] {
			t.Fatalf("single-column chain must share x: %s x=%v, want %v\n%s", ids[i], xs[ids[i]], xs[ids[0]], svg)
		}
	}
	box := viewBoxOf(t, svg)
	if box[3] <= box[2] {
		t.Fatalf("linear graph must be taller than wide: %v x %v\n%s", box[2], box[3], svg)
	}
}

func TestDOTRankDirTB(t *testing.T) {
	dot := DOT(workflowGraph())
	if !strings.Contains(dot, "rankdir=TB;") {
		t.Fatalf("rankdir=TB missing:\n%s", dot)
	}
	if strings.Contains(dot, "rankdir=LR") {
		t.Fatalf("rankdir=LR must be gone:\n%s", dot)
	}
}

// Парсеры разметки двух рендеров: сравнение ведём по меткам узлов.
var (
	reSVGNode      = regexp.MustCompile(`<g class="node [^"]*"><rect x="([-0-9.]+)" y="([-0-9.]+)" width="([-0-9.]+)" height="([-0-9.]+)"[^>]*/><text x="([-0-9.]+)" y="([-0-9.]+)"[^>]*>([^<]*)</text></g>`)
	reSVGEdgeLabel = regexp.MustCompile(`<text class="edge-label" x="([-0-9.]+)" y="([-0-9.]+)"[^>]*>([^<]*)</text>`)
	reSVGPolyline  = regexp.MustCompile(`<polyline class="edge[^"]*" points="([^"]+)"/>`)
	reDOTNode      = regexp.MustCompile(`(?m)^\s*(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_]*)) \[label="((?:[^"\\]|\\.)*)", shape=[A-Za-z]+\];$`)
	reDOTEdge      = regexp.MustCompile(`(?m)^\s*(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_]*)) -> (?:"([^"]+)"|([A-Za-z0-9_]*))(?: \[label="((?:[^"\\]|\\.)*)"\])?;$`)
)

var xmlUnescape = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&amp;", "&")

type nodeBox struct {
	x, y, w, h float64
}

type renderedGraph struct {
	nodes    map[string]nodeBox
	edges    [][2]string
	captions []string
}

func dotID(groups []string) string {
	for _, g := range groups {
		if g != "" {
			return g
		}
	}
	return ""
}

func dotModel(t *testing.T, dot string) renderedGraph {
	t.Helper()
	m := renderedGraph{nodes: map[string]nodeBox{}}
	for _, match := range reDOTNode.FindAllStringSubmatch(dot, -1) {
		m.nodes[match[3]] = nodeBox{}
	}
	for _, match := range reDOTEdge.FindAllStringSubmatch(dot, -1) {
		m.edges = append(m.edges, [2]string{dotID(match[1:3]), dotID(match[3:5])})
		if match[5] != "" {
			m.captions = append(m.captions, match[5])
		}
	}
	if len(m.nodes) == 0 {
		t.Fatalf("no DOT nodes parsed:\n%s", dot)
	}
	return m
}

// labelOf — отображение ID узлов на их label: DOT говорит о рёбрах
// в терминах ID, SVG — в терминах label.
func labelOf(g Graph) map[string]string {
	m := make(map[string]string, len(g.Nodes))
	for _, n := range g.Nodes {
		m[n.ID] = n.Label
	}
	return m
}

func dotModelLabeled(t *testing.T, dot string, g Graph) renderedGraph {
	t.Helper()
	m := dotModel(t, dot)
	labels := labelOf(g)
	for i, e := range m.edges {
		for j := range e {
			if l, ok := labels[e[j]]; ok {
				m.edges[i][j] = l
			}
		}
	}
	return m
}

// svgModel восстанавливает граф из разметки: концы каждой полилинии
// относятся к ближайшему узлу, подписи рёбер читаются напрямую.
func svgModel(t *testing.T, g Graph, svg string) renderedGraph {
	t.Helper()
	m := renderedGraph{nodes: map[string]nodeBox{}}
	centers := map[string][2]float64{}
	for _, match := range reSVGNode.FindAllStringSubmatch(svg, -1) {
		vals := make([]float64, 6)
		for i := range vals {
			v, err := strconv.ParseFloat(match[i+1], 64)
			if err != nil {
				t.Fatalf("parse node coordinate %q: %v", match[i+1], err)
			}
			vals[i] = v
		}
		label := xmlUnescape.Replace(match[7])
		m.nodes[label] = nodeBox{vals[0], vals[1], vals[2], vals[3]}
		centers[label] = [2]float64{vals[4], vals[5] - 5}
	}
	if len(m.nodes) == 0 {
		t.Fatalf("no SVG nodes parsed:\n%s", svg)
	}
	maxLabel := 0
	for _, n := range g.Nodes {
		if len(n.Label) > maxLabel {
			maxLabel = len(n.Label)
		}
	}
	reach := float64(maxLabel)*svgGlyphWidth + 2*svgTextPad
	if reach < 96 {
		reach = 96
	}
	nearest := func(p [2]float64) string {
		best, bestD := "", math.MaxFloat64
		for label, c := range centers {
			d := math.Hypot(p[0]-c[0], p[1]-c[1])
			if d < bestD {
				best, bestD = label, d
			}
		}
		if bestD > reach {
			t.Fatalf("polyline endpoint %v matches no node (nearest %s at %v):\n%s", p, best, bestD, svg)
		}
		return best
	}
	for _, match := range reSVGPolyline.FindAllStringSubmatch(svg, -1) {
		fields := strings.Fields(match[1])
		if len(fields) < 2 {
			t.Fatalf("degenerate polyline: %q", match[1])
		}
		parse := func(s string) [2]float64 {
			xs := strings.SplitN(s, ",", 2)
			var p [2]float64
			p[0], _ = strconv.ParseFloat(xs[0], 64)
			p[1], _ = strconv.ParseFloat(xs[1], 64)
			return p
		}
		m.edges = append(m.edges, [2]string{nearest(parse(fields[0])), nearest(parse(fields[len(fields)-1]))})
	}
	for _, match := range reSVGEdgeLabel.FindAllStringSubmatch(svg, -1) {
		m.captions = append(m.captions, xmlUnescape.Replace(match[3]))
	}
	return m
}

func sortedPairs(pairs [][2]string) []string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p[0]+"->"+p[1])
	}
	sort.Strings(out)
	return out
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// BUG: DOT and SVG renderers can drift apart and show different graphs.
// HYPOTHESIS: Nothing compares the two outputs.
// SOLUTION: Parse both renderings back into nodes, edges and captions and compare the sets.
// RESULT: PENDING
// STATUS: RED
func TestSVGAndDOTShareGraph(t *testing.T) {
	for name, g := range map[string]Graph{"sample": sampleGraph(), "workflow": workflowGraph(), "cycle": cycleGraph()} {
		dm := dotModelLabeled(t, DOT(g), g)
		sm := svgModel(t, g, SVG(RunView{Graph: g}))
		if len(dm.nodes) != len(sm.nodes) {
			t.Fatalf("%s: node count differs: DOT %d, SVG %d", name, len(dm.nodes), len(sm.nodes))
		}
		for label := range dm.nodes {
			if _, ok := sm.nodes[label]; !ok {
				t.Fatalf("%s: node %q missing in SVG", name, label)
			}
		}
		if got, want := sortedPairs(sm.edges), sortedPairs(dm.edges); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: edges differ:\nSVG: %v\nDOT: %v", name, got, want)
		}
		if got, want := sortedStrings(sm.captions), sortedStrings(dm.captions); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: captions differ:\nSVG: %v\nDOT: %v", name, got, want)
		}
	}
}

// BUG: Bypass edges cut straight through the rows between their ends.
// HYPOTHESIS: Only adjacent rows have routing; longer edges reuse the node axis.
// SOLUTION: Route non-adjacent edges through vertical channels outside occupied node spans.
// RESULT: PENDING
// STATUS: RED
func TestSVGEdgesAvoidNodeRows(t *testing.T) {
	for name, g := range map[string]Graph{"workflow": workflowGraph(), "sample": sampleGraph(), "cycle": cycleGraph()} {
		svg := SVG(RunView{Graph: g})
		sm := svgModel(t, g, svg)
		for _, match := range reSVGPolyline.FindAllStringSubmatch(svg, -1) {
			fields := strings.Fields(match[1])
			var prev [2]float64
			for i, f := range fields {
				xs := strings.SplitN(f, ",", 2)
				var p [2]float64
				p[0], _ = strconv.ParseFloat(xs[0], 64)
				p[1], _ = strconv.ParseFloat(xs[1], 64)
				if i > 0 {
					if p[0] != prev[0] && p[1] != prev[1] {
						t.Fatalf("%s: polyline segment %v->%v is not axis aligned:\n%s", name, prev, p, svg)
					}
					for label, box := range sm.nodes {
						// Полоса узла сжимается на 2 px: касание границы
						// источника и цели допустимо, проход сквозь — нет.
						in := box.x + 2
						iw := box.x + box.w - 2
						iy := box.y + 2
						ih := box.y + box.h - 2
						lo, hi := prev, p
						if lo[0] > hi[0] || lo[1] > hi[1] {
							lo, hi = hi, lo
						}
						overlap := lo[0] < iw && hi[0] > in && lo[1] < ih && hi[1] > iy
						if overlap {
							t.Fatalf("%s: edge passes through node %q rect %v, segment %v->%v:\n%s", name, label, box, prev, p, svg)
						}
					}
				}
				prev = p
			}
		}
	}
}

// BUG: Two edge captions landing on the same point overlap and become unreadable.
// HYPOTHESIS: Label positions depend on edge geometry only, not on neighbours.
// SOLUTION: Separate captions that share a row by pushing them apart along x.
// RESULT: PENDING
// STATUS: RED
func TestSVGEdgeLabelsSeparated(t *testing.T) {
	// Параллельные рёбра одной пары узлов дают подписи в одной точке.
	g := Graph{
		Nodes: []Node{{ID: "a", Label: "a"}, {ID: "b", Label: "b"}},
		Edges: []Edge{{From: "a", To: "b", Label: "yes"}, {From: "a", To: "b", Condition: "no"}},
	}
	for name, graph := range map[string]Graph{"parallel": g, "workflow": workflowGraph(), "sample": sampleGraph()} {
		checkLabelSeparation(t, name, SVG(RunView{Graph: graph}))
	}
}

func checkLabelSeparation(t *testing.T, name, svg string) {
	t.Helper()
	matches := reSVGEdgeLabel.FindAllStringSubmatch(svg, -1)
	if len(matches) < 2 {
		t.Fatalf("%s: need at least two edge labels:\n%s", name, svg)
	}
	coord := func(i int) (float64, float64, string) {
		x, _ := strconv.ParseFloat(matches[i][1], 64)
		y, _ := strconv.ParseFloat(matches[i][2], 64)
		return x, y, matches[i][3]
	}
	for i := 0; i < len(matches); i++ {
		xi, yi, ci := coord(i)
		for j := i + 1; j < len(matches); j++ {
			xj, yj, cj := coord(j)
			if math.Abs(yi-yj) < 14 && math.Abs(xi-xj) < 24 {
				t.Fatalf("%s: captions %q (%v,%v) and %q (%v,%v) overlap:\n%s", name, ci, xi, yi, cj, xj, yj, svg)
			}
		}
	}
}

// BUG: Hardcoded fill="white" stroke="black" break the dark theme.
// HYPOTHESIS: Presentation attributes cannot be themed by the page.
// SOLUTION: Drop color attributes; an embedded style drives paint through currentColor and --pip-* variables.
// RESULT: PENDING
// STATUS: RED
func TestSVGThemeable(t *testing.T) {
	svg := SVG(RunView{Graph: sampleGraph()})
	start := strings.Index(svg, "<style>")
	end := strings.Index(svg, "</style>")
	if start < 0 || end < start {
		t.Fatalf("embedded style missing:\n%s", svg)
	}
	body := svg[:start] + svg[end+len("</style>"):]
	if strings.Contains(body, `fill="`) || strings.Contains(body, `stroke="`) {
		t.Fatalf("hardcoded color attribute in markup:\n%s", body)
	}
	if hex := regexp.MustCompile(`#[0-9a-fA-F]{3,8}`).FindString(body); hex != "" {
		t.Fatalf("hardcoded color %s in markup:\n%s", hex, body)
	}
	style := svg[start:end]
	for _, want := range []string{
		"currentColor", "var(--pip-", "@media (prefers-color-scheme: dark)",
		".node-actor", ".node-human", ".node-exit",
		".state-active", ".state-done", ".state-failed",
	} {
		if !strings.Contains(style, want) {
			t.Fatalf("style rule %s missing:\n%s", want, style)
		}
	}
}

// FEATURE: Node kinds stay visible in SVG markup.
// SPEC: actor, human and exit nodes carry their kind class so styling can distinguish them.
// RESULT: PENDING
// STATUS: RED
func TestSVGNodeKindClasses(t *testing.T) {
	svg := SVG(RunView{Graph: sampleGraph()})
	for _, want := range []string{`class="node node-start`, `class="node node-actor`, `class="node node-human`, `class="node node-exit`} {
		if !strings.Contains(svg, want) {
			t.Fatalf("%s missing:\n%s", want, svg)
		}
	}
}

// BUG: Подпись обходной дорожки слева ложится в полосу узлов и скрыта rect.
// HYPOTHESIS: подпись обязана уходить в сторону, свободную от узлов.
// SOLUTION: channelLabel проверяет прямоугольник подписи против узлов.
func TestSVGChannelLabelNotHiddenByNode(t *testing.T) {
	// Цепочка + два параллельных обхода: правая дорожка занята первым,
	// второй уходит влево — его подпись не должна попасть под узлы.
	g := Graph{Nodes: []Node{
		{ID: "a", Label: "a", Kind: "actor"},
		{ID: "b", Label: "b", Kind: "actor"},
		{ID: "c", Label: "c", Kind: "actor"},
		{ID: "d", Label: "d", Kind: "actor"},
		{ID: "e", Label: "e", Kind: "exit"},
	}, Edges: []Edge{
		{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "d"}, {From: "d", To: "e"},
		{From: "a", To: "e", Label: "outcome=fail"}, {From: "b", To: "e", Label: "outcome=abort"},
	}}
	svg := SVG(RunView{Graph: g})
	nodes := reSVGNode.FindAllStringSubmatch(svg, -1)
	labels := reSVGEdgeLabel.FindAllStringSubmatch(svg, -1)
	if len(nodes) == 0 || len(labels) == 0 {
		t.Fatalf("empty render:\n%s", svg)
	}
	for _, lm := range labels {
		lx, _ := strconv.ParseFloat(lm[1], 64)
		ly, _ := strconv.ParseFloat(lm[2], 64)
		text := xmlUnescape.Replace(lm[3])
		half := float64(len([]rune(text))) * 3.0
		for _, nm := range nodes {
			nx, _ := strconv.ParseFloat(nm[1], 64)
			ny, _ := strconv.ParseFloat(nm[2], 64)
			nw, _ := strconv.ParseFloat(nm[3], 64)
			nh, _ := strconv.ParseFloat(nm[4], 64)
			if lx+half > nx && lx-half < nx+nw && ly+7 > ny && ly-7 < ny+nh {
				t.Fatalf("edge label %q at (%.0f,%.0f) overlaps node %s at (%.0f,%.0f)", text, lx, ly, nm[7], nx, ny)
			}
		}
	}
}
