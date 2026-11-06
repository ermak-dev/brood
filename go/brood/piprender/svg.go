package piprender

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Константы раскладки в пользовательских единицах. Блоки узлов
// одинаковой высоты; ширина едина для графа и вмещает самый длинный
// label при оценке ширины символа.
const (
	svgMargin     = 24.0 // отступ содержимого от края viewBox
	svgGapX       = 96.0 // горизонтальный зазор между слоями
	svgGapY       = 48.0 // вертикальный зазор между узлами слоя
	svgTextPad    = 16.0 // горизонтальный запас текста в блоке
	svgGlyphWidth = 8.0  // оценка ширины символа label
	svgBoxHeight  = 40.0 // высота блока узла
	svgArrowSize  = 10.0 // катет наконечника ребра
)

// escape renders text for XML character data and attribute values.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			if r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0xfffe || r == 0xffff {
				b.WriteRune('�')
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// classToken keeps untrusted values within a single CSS class.
func classToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "_%x_", r)
		}
	}
	return b.String()
}

// num formats a float coordinate without trailing zeros.
func num(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// layers assigns longest-path layers after removing DFS back edges. Sorting
// makes the cycle breaking and layout independent of input order.
func layers(g Graph) [][]string {
	layer := make(map[string]int, len(g.Nodes))
	for _, n := range g.Nodes {
		layer[n.ID] = 0
	}
	adj := make(map[string][]string)
	for _, e := range sortedEdges(g.Edges) {
		_, from := layer[e.From]
		_, to := layer[e.To]
		if from && to {
			adj[e.From] = append(adj[e.From], e.To)
		}
	}
	ids := make([]string, 0, len(layer))
	for id := range layer {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	starts := make(map[string]bool)
	for _, n := range g.Nodes {
		if n.Kind == KindStart {
			starts[n.ID] = true
		}
	}
	sort.SliceStable(ids, func(i, j int) bool { return starts[ids[i]] && !starts[ids[j]] })
	visited := make(map[string]int)
	var order []string
	var visit func(string)
	visit = func(id string) {
		visited[id] = 1
		for _, next := range adj[id] {
			if visited[next] == 0 {
				visit(next)
			}
		}
		visited[id] = 2
		order = append(order, id)
	}
	for _, id := range ids {
		if visited[id] == 0 {
			visit(id)
		}
	}
	// Only edges pointing forward in reverse postorder participate.
	rank := make(map[string]int, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		rank[order[i]] = len(order) - 1 - i
	}
	for i := len(order) - 1; i >= 0; i-- {
		id := order[i]
		for _, next := range adj[id] {
			if rank[id] < rank[next] && layer[next] < layer[id]+1 {
				layer[next] = layer[id] + 1
			}
		}
	}
	byLayer := make(map[int][]string, len(layer))
	for _, n := range g.Nodes {
		l := layer[n.ID]
		byLayer[l] = append(byLayer[l], n.ID)
	}
	keys := make([]int, 0, len(byLayer))
	for k := range byLayer {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	cols := make([][]string, 0, len(keys))
	for _, k := range keys {
		ids := byLayer[k]
		sort.Strings(ids)
		cols = append(cols, ids)
	}
	return cols
}

// SVG renders the run view as standalone SVG with a hand-rolled layout:
// longest-path layering left to right, nodes spread vertically inside a
// layer, polyline edges with arrowheads, viewBox sized to content.
// Every node group carries class "node node-<kind> state-<state>" for
// CSS styling in the web panel. Output is deterministic. A zero view
// renders the graph with all states pending.
func SVG(v RunView) string {
	g := v.Graph
	cols := layers(g)

	label := make(map[string]string, len(g.Nodes))
	kind := make(map[string]Kind, len(g.Nodes))
	state := make(map[string]State, len(g.Nodes))
	maxLabel := 0
	for _, n := range g.Nodes {
		label[n.ID] = n.Label
		kind[n.ID] = n.Kind
		if len(n.Label) > maxLabel {
			maxLabel = len(n.Label)
		}
		if st, ok := v.Nodes[n.ID]; ok {
			state[n.ID] = st
		} else {
			state[n.ID] = StatePending
		}
	}
	boxW := float64(maxLabel)*svgGlyphWidth + 2*svgTextPad
	if boxW < 96 {
		boxW = 96
	}

	// Центры узлов: слои слева направо, внутри слоя — сверху вниз по ID.
	pos := make(map[string][2]float64, len(g.Nodes))
	x := svgMargin + boxW/2
	for _, col := range cols {
		y := svgMargin + svgBoxHeight/2
		for _, id := range col {
			pos[id] = [2]float64{x, y}
			y += svgBoxHeight + svgGapY
		}
		x += boxW + svgGapX
	}
	width := x - svgGapX + svgMargin
	height := svgMargin
	for _, col := range cols {
		if n := len(col); n > 0 {
			if bottom := svgMargin + float64(n)*svgBoxHeight + float64(n-1)*svgGapY; bottom > height {
				height = bottom
			}
		}
	}
	height += svgMargin

	minX, minY, maxX, maxY := 0.0, 0.0, width, height
	for _, e := range g.Edges {
		from, okFrom := pos[e.From]
		to, okTo := pos[e.To]
		if !okFrom || !okTo {
			continue
		}
		x1, x2 := from[0]+boxW/2, to[0]-boxW/2
		if caption := edgeLabel(e); caption != "" {
			labelX, labelY := (x1+x2)/2, from[1]-6
			if x2 < x1 {
				labelY = min(from[1], to[1]) - svgBoxHeight/2 - svgMargin/2
			}
			half := float64(utf8.RuneCountInString(caption)) * 16 / 2
			minX = min(minX, labelX-half-svgMargin)
			maxX = max(maxX, labelX+half+svgMargin)
			minY = min(minY, labelY-16)
			maxY = max(maxY, labelY+svgMargin)
		}
		if x2 < x1 {
			maxX = max(maxX, max(x1, to[0]+boxW/2)+svgMargin/2+1)
			minY = min(minY, min(from[1], to[1])-svgBoxHeight/2-svgMargin/2-1)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="%s %s %s %s" role="img">`+"\n", num(minX), num(minY), num(maxX-minX), num(maxY-minY))
	// Рёбра первыми: линии лежат под узлами и подписями. Порядок
	// сортировки DOT (From, To) держит вывод независимым от входного
	// порядка рёбер; состояние берётся по индексу в g.Edges.
	ordered := make([]int, len(g.Edges))
	for i := range ordered {
		ordered[i] = i
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := g.Edges[ordered[i]], g.Edges[ordered[j]]
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	for _, index := range ordered {
		e := g.Edges[index]
		from, okFrom := pos[e.From]
		to, okTo := pos[e.To]
		if !okFrom || !okTo {
			continue
		}
		st, ok := v.Edges[index]
		if !ok {
			st = StatePending
		}
		// Полилиния: правый край источника, горизонтальный пробег,
		// вертикальный вход в цель. Наконечник лежит на границе цели.
		x1, y1 := from[0]+boxW/2, from[1]
		x2 := to[0] - boxW/2
		y2 := to[1]
		if caption := edgeLabel(e); caption != "" {
			labelX, labelY := (x1+x2)/2, y1-6
			if x2 < x1 {
				labelY = min(y1, y2) - svgBoxHeight/2 - svgMargin/2
			}
			fmt.Fprintf(&b, `<text class="edge-label" x="%s" y="%s" text-anchor="middle">%s</text>`+"\n", num(labelX), num(labelY), escape(caption))
		}
		if x2 < x1 {
			// Обратные рёбра и петли обходят узлы сверху.
			x2 = to[0] + boxW/2
			top := min(y1, y2) - svgBoxHeight/2 - svgMargin/2
			fmt.Fprintf(&b, `<polyline class="edge edge-%s" points="%s,%s %s,%s %s,%s %s,%s %s,%s %s,%s" fill="none" stroke="black"/>`+"\n",
				classToken(string(st)), num(x1), num(y1), num(x1+svgMargin/2), num(y1), num(x1+svgMargin/2), num(top), num(x2+svgMargin/2), num(top), num(x2+svgMargin/2), num(y2), num(x2), num(y2))
			fmt.Fprintf(&b, `<polygon class="edge-arrow edge-%s" points="%s,%s %s,%s %s,%s" fill="black"/>`+"\n", classToken(string(st)), num(x2), num(y2), num(x2+svgArrowSize), num(y2-svgArrowSize/2), num(x2+svgArrowSize), num(y2+svgArrowSize/2))
			continue
		} else if y2 != y1 {
			if y2 > y1 {
				y2 = to[1] - svgBoxHeight/2
			} else {
				y2 = to[1] + svgBoxHeight/2
			}
		}
		fmt.Fprintf(&b, `<polyline class="edge edge-%s" points="%s,%s %s,%s %s,%s" fill="none" stroke="black"/>`+"\n",
			classToken(string(st)), num(x1), num(y1), num(x2), num(y1), num(x2), num(y2))
		// Направление входа в цель: вертикально, а при вырожденном
		// вертикальном сегменте — горизонтально.
		var arrow string
		switch {
		case y2 > y1:
			arrow = fmt.Sprintf("%s,%s %s,%s %s,%s",
				num(x2), num(y2), num(x2-svgArrowSize/2), num(y2-svgArrowSize), num(x2+svgArrowSize/2), num(y2-svgArrowSize))
		case y2 < y1:
			arrow = fmt.Sprintf("%s,%s %s,%s %s,%s",
				num(x2), num(y2), num(x2-svgArrowSize/2), num(y2+svgArrowSize), num(x2+svgArrowSize/2), num(y2+svgArrowSize))
		case x2 >= x1:
			arrow = fmt.Sprintf("%s,%s %s,%s %s,%s",
				num(x2), num(y2), num(x2-svgArrowSize), num(y2-svgArrowSize/2), num(x2-svgArrowSize), num(y2+svgArrowSize/2))
		default:
			arrow = fmt.Sprintf("%s,%s %s,%s %s,%s",
				num(x2), num(y2), num(x2+svgArrowSize), num(y2-svgArrowSize/2), num(x2+svgArrowSize), num(y2+svgArrowSize/2))
		}
		fmt.Fprintf(&b, `<polygon class="edge-arrow edge-%s" points="%s" fill="black"/>`+"\n", classToken(string(st)), arrow)
	}
	for _, col := range cols {
		for _, id := range col {
			p := pos[id]
			rx := "0"
			if kind[id] == KindStart {
				rx = "20"
			}
			fmt.Fprintf(&b, `<g class="node node-%s state-%s"><rect x="%s" y="%s" width="%s" height="%s" rx="%s" fill="white" stroke="black"/><text x="%s" y="%s" text-anchor="middle">%s</text></g>`+"\n",
				classToken(string(kind[id])), classToken(string(state[id])),
				num(p[0]-boxW/2), num(p[1]-svgBoxHeight/2), num(boxW), num(svgBoxHeight), rx,
				num(p[0]), num(p[1]+5), escape(label[id]))
		}
	}
	b.WriteString("</svg>\n")
	return b.String()
}
