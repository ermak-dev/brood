package piprender

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Константы раскладки в пользовательских единицах. Слои идут строками
// сверху вниз, узлы строки — колонками слева направо по ID. Блоки узлов
// одинаковой высоты; ширина едина для графа и вмещает самый длинный
// label при оценке ширины символа.
const (
	svgMargin     = 24.0 // отступ содержимого от края viewBox
	svgGapX       = 64.0 // горизонтальный зазор между узлами строки
	svgGapY       = 80.0 // вертикальный зазор между строками
	svgTextPad    = 16.0 // горизонтальный запас текста в блоке
	svgGlyphWidth = 8.0  // оценка ширины символа label
	svgBoxHeight  = 40.0 // высота блока узла
	svgArrowSize  = 10.0 // катет наконечника ребра
	svgChannel    = 12.0 // шаг поиска свободной вертикальной дорожки
)

// pipStyle — встроенные правила SVG. Краски берутся из currentColor и
// переменных --pip-* со светлыми дефолтами; запрос prefers-color-scheme
// переключает фон узла для тёмной схемы. Страница задаёт цвет текста
// селектором контейнера и может переопределить любую переменную.
// Панель (#214) этот <style> не исполняет: она красит граф своим
// CSS-модулем, а таблица остаётся для автономного SVG.
const pipStyle = `<style>` + "\n" +
	`.node rect{fill:var(--pip-node-fill,#fff);stroke:currentColor}` + "\n" +
	`.node text{fill:currentColor}` + "\n" +
	`.node-actor rect{stroke-width:1.5}` + "\n" +
	`.node-human rect{stroke-dasharray:6 3;stroke-width:2}` + "\n" +
	`.node-exit rect{stroke-width:2.5}` + "\n" +
	`.edge{fill:none;stroke:currentColor;stroke-opacity:.55}` + "\n" +
	`.edge-arrow{fill:currentColor;fill-opacity:.55}` + "\n" +
	`.edge-label{fill:currentColor;font-size:12px}` + "\n" +
	`.state-active rect{fill:var(--pip-active-fill,#fef3c7);stroke:var(--pip-active-stroke,#b45309)}` + "\n" +
	`.state-done rect{fill:var(--pip-done-fill,#dcfce7);stroke:var(--pip-done-stroke,#15803d)}` + "\n" +
	`.state-failed rect{fill:var(--pip-failed-fill,#fee2e2);stroke:var(--pip-failed-stroke,#b91c1c)}` + "\n" +
	`.state-skipped rect{fill:var(--pip-skipped-fill,#e5e7eb);stroke:var(--pip-skipped-stroke,#6b7280)}` + "\n" +
	`svg:root{color:var(--pip-text,#111827)}` + "\n" +
	`@media (prefers-color-scheme: dark){svg:root{color:var(--pip-text-dark,#e5e7eb);background:var(--pip-bg-dark,#0f172a)}.node.state-pending rect{fill:var(--pip-node-fill-dark,#1e293b)}.state-skipped rect{fill:var(--pip-skipped-fill-dark,#374151)}.state-active text,.state-done text,.state-failed text{fill:var(--pip-state-text,#111827)}}` + "\n" +
	`</style>`

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

// halfCaption оценивает половину ширины подписи в пользовательских
// единицах; подписи рёбер набраны 12 px против 16 px у узлов.
func halfCaption(caption string) float64 {
	return float64(utf8.RuneCountInString(caption)) * svgGlyphWidth * 12 / 16 / 2
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
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		ids := byLayer[k]
		sort.Strings(ids)
		rows = append(rows, ids)
	}
	return rows
}

// svgRect — прямоугольник узла в координатах SVG.
type svgRect struct {
	x, y, w, h float64
}

// point — точка полилинии.
type point [2]float64

// svgRoute — маршрут ребра: полилиния, наконечник и подпись.
type svgRoute struct {
	st       State
	pts      []point
	tip      point
	caption  string
	labelX   float64
	labelY   float64
	hasLabel bool
}

// svgLayout держит всё, что нужно маршрутам: центры и блоки узлов,
// строку каждого узла и счётчик занятых обходных дорожек.
type svgLayout struct {
	boxW     float64
	pos      map[string]point
	rowOf    map[string]int
	rects    map[string]svgRect
	order    []string // ID узлов в порядке строк
	occupied map[float64]int
}

// hitsNode сообщает, задевает ли отрезок a-b блок какого-либо узла,
// кроме концов ребра. Запас 2 px отличает касание границы источника
// и цели от прохода сквозь узел.
func (l *svgLayout) hitsNode(a, b point, skipFrom, skipTo string) bool {
	lo, hi := a, b
	if lo[0] > hi[0] || lo[1] > hi[1] {
		lo, hi = hi, lo
	}
	for id, r := range l.rects {
		if id == skipFrom || id == skipTo {
			continue
		}
		inX, inY := r.x+2, r.y+2
		iw, ih := r.x+r.w-2, r.y+r.h-2
		if lo[0] < iw && hi[0] > inX && lo[1] < ih && hi[1] > inY {
			return true
		}
	}
	return false
}

// routeHitsNode проверяет весь маршрут ребра против узлов.
func (l *svgLayout) routeHitsNode(pts []point, from, to string) bool {
	for i := 1; i < len(pts); i++ {
		if l.hitsNode(pts[i-1], pts[i], from, to) {
			return true
		}
	}
	return false
}

// channel ищет свободную вертикальную дорожку для обходного ребра:
// кандидаты между узлами и у их боков, ближайшие к середине; занятые
// другими обходами и задевающие узлы пропускаются, внешний запас
// уходит в сторону, пока дорожка не найдётся.
func (l *svgLayout) channel(from, to point, fromID, toID string, build func(float64) ([]point, point)) (float64, []point, point) {
	mid := (from[0] + to[0]) / 2
	var cands []float64
	seen := map[float64]bool{}
	add := func(x float64) {
		if !seen[x] {
			seen[x] = true
			cands = append(cands, x)
		}
	}
	lo, hi := from[0], to[0]
	if lo > hi {
		lo, hi = hi, lo
	}
	for x := lo + l.boxW/2 + svgChannel; x < hi-l.boxW/2; x += svgChannel {
		add(x)
	}
	add(from[0] - l.boxW/2 - svgChannel)
	add(from[0] + l.boxW/2 + svgChannel)
	add(to[0] - l.boxW/2 - svgChannel)
	add(to[0] + l.boxW/2 + svgChannel)
	sort.SliceStable(cands, func(i, j int) bool {
		di, dj := cands[i]-mid, cands[j]-mid
		if di < 0 {
			di = -di
		}
		if dj < 0 {
			dj = -dj
		}
		if di != dj {
			return di < dj
		}
		return cands[i] > cands[j] // при равенстве — правая сторона
	})
	for _, x := range cands {
		if l.occupied[x] > 0 {
			continue
		}
		pts, tip := build(x)
		if !l.routeHitsNode(pts, fromID, toID) {
			return x, pts, tip
		}
	}
	// Внешний запас: шагаем от самого правого и левого кандидата,
	// пока полилиния не освободится. Граф конечен, узлы тоже.
	right := l.rightEdge() + svgChannel
	left := l.leftEdge() - svgChannel
	for i := 0; i < 256; i++ {
		for _, x := range []float64{right, left} {
			if l.occupied[x] > 0 {
				continue
			}
			pts, tip := build(x)
			if !l.routeHitsNode(pts, fromID, toID) {
				return x, pts, tip
			}
		}
		right += svgChannel
		left -= svgChannel
	}
	// Недостижимо для конечного графа; последний кандидат лучше, чем
	// отказ от ребра.
	pts, tip := build(right)
	return right, pts, tip
}

func (l *svgLayout) rightEdge() float64 {
	max := 0.0
	for _, r := range l.rects {
		if r.x+r.w > max {
			max = r.x + r.w
		}
	}
	return max
}

// channelLabel ставит подпись обходной дорожки: по умолчанию правее
// дорожки, но если подпись ложится на строку узлов — уходит в сторону
// от узлов с запасом. Возвращает точку центра подписи.
func (l *svgLayout) channelLabel(ch, half, y float64) (float64, float64) {
	x := ch + svgChannel/2 + half
	if !l.labelHitsNode(x, half, y) {
		return x, y
	}
	for _, x2 := range []float64{ch - svgChannel/2 - half, l.rightEdge() + svgChannel + half, l.leftEdge() - svgChannel - half} {
		if !l.labelHitsNode(x2, half, y) {
			return x2, y
		}
	}
	return x, y
}

// labelHitsNode проверяет пересечение прямоугольника подписи с узлами.
func (l *svgLayout) labelHitsNode(x, half, y float64) bool {
	for _, r := range l.rects {
		if x+half > r.x && x-half < r.x+r.w && y+7 > r.y && y-7 < r.y+r.h {
			return true
		}
	}
	return false
}

func (l *svgLayout) leftEdge() float64 {
	min := 0.0
	first := true
	for _, r := range l.rects {
		if first || r.x < min {
			min = r.x
			first = false
		}
	}
	return min
}

// route строит маршрут ребра. Соседние строки соединяет прямой от
// нижней грани источника к верхней грани цели; обходные и обратные
// рёбра ведёт через свободную вертикальную дорожку сбоку от узлов.
func (l *svgLayout) route(e Edge, st State) (svgRoute, bool) {
	from, okFrom := l.pos[e.From]
	to, okTo := l.pos[e.To]
	if !okFrom || !okTo {
		return svgRoute{}, false
	}
	r := svgRoute{st: st, caption: edgeLabel(e)}
	switch {
	case l.rowOf[e.To] == l.rowOf[e.From]+1:
		// Ортогональный переход в следующую строку: вниз до середины
		// зазора, горизонталь, вход в верхнюю грань цели.
		p0 := point{from[0], from[1] + svgBoxHeight/2}
		mid := (from[1] + svgBoxHeight/2 + to[1] - svgBoxHeight/2) / 2
		p1 := point{to[0], to[1] - svgBoxHeight/2 - 1}
		if from[0] == to[0] {
			r.pts = []point{p0, p1}
		} else {
			r.pts = []point{p0, {from[0], mid}, {to[0], mid}, p1}
		}
		r.tip = point{to[0], to[1] - svgBoxHeight/2}
		if r.caption != "" {
			r.hasLabel = true
			edgeX := max(from[0], to[0])
			r.labelX = edgeX + svgChannel + halfCaption(r.caption)
			r.labelY = mid + 4
		}
	case l.rowOf[e.To] == l.rowOf[e.From]:
		// Соседи в одной строке: горизонтальный пробег через дорожку.
		build := func(ch float64) ([]point, point) {
			exit := point{from[0] + l.boxW/2, from[1]}
			tip := point{to[0] - l.boxW/2, to[1]}
			if to[0] < from[0] {
				exit = point{from[0] - l.boxW/2, from[1]}
				tip = point{to[0] + l.boxW/2, to[1]}
			}
			return []point{exit, {ch, from[1]}, tip}, tip
		}
		ch, pts, tip := l.channel(from, to, e.From, e.To, build)
		l.occupied[ch]++
		r.pts, r.tip = pts, tip
		if r.caption != "" {
			r.hasLabel = true
			r.labelX = (from[0] + to[0]) / 2
			r.labelY = from[1] - svgBoxHeight/2 - 6
		}
	case l.rowOf[e.To] < l.rowOf[e.From]:
		// Обратное ребро: выход верхней гранью, обход над строками,
		// вход в нижнюю грань цели.
		build := func(ch float64) ([]point, point) {
			exit := point{from[0], from[1] - svgBoxHeight/2}
			lift := exit[1] - 6
			approach := to[1] + svgBoxHeight/2 + svgArrowSize
			return []point{exit, {from[0], lift}, {ch, lift}, {ch, approach}, {to[0], approach}}, point{to[0], to[1] + svgBoxHeight/2}
		}
		ch, pts, tip := l.channel(from, to, e.From, e.To, build)
		l.occupied[ch]++
		r.pts, r.tip = pts, tip
		if r.caption != "" {
			r.hasLabel = true
			lo, hi := pts[1][1], pts[3][1]
			r.labelX, r.labelY = l.channelLabel(ch, halfCaption(r.caption), (lo+hi)/2+4)
		}
	default:
		// Дальний переход вперёд: выход боковой гранью, вертикальная
		// дорожка в стороне от строк, вход в верхнюю грань цели.
		build := func(ch float64) ([]point, point) {
			side := 1.0
			if ch < from[0] {
				side = -1
			}
			exit := point{from[0] + side*l.boxW/2, from[1]}
			approach := to[1] - svgBoxHeight/2 - svgArrowSize
			return []point{exit, {ch, from[1]}, {ch, approach}, {to[0], approach}}, point{to[0], to[1] - svgBoxHeight/2}
		}
		ch, pts, tip := l.channel(from, to, e.From, e.To, build)
		l.occupied[ch]++
		r.pts, r.tip = pts, tip
		if r.caption != "" {
			r.hasLabel = true
			lo, hi := pts[1][1], pts[3][1]
			r.labelX, r.labelY = l.channelLabel(ch, halfCaption(r.caption), (lo+hi)/2+4)
		}
	}
	return r, true
}

// separateLabels разводит подписи, попавшие в одну строку: каждая
// следующая отодвигается вправо за предыдущую с запасом половин
// ширины. Порядок фиксирован порядком рёбер, вывод детерминирован.
func separateLabels(routes []svgRoute) {
	halves := make([]float64, len(routes))
	for i, r := range routes {
		halves[i] = halfCaption(r.caption)
	}
	for i := range routes {
		if !routes[i].hasLabel {
			continue
		}
		for moved := true; moved; {
			moved = false
			for j := 0; j < i; j++ {
				if !routes[j].hasLabel {
					continue
				}
				dy := routes[i].labelY - routes[j].labelY
				if dy < 0 {
					dy = -dy
				}
				if dy >= 16 {
					continue
				}
				gap := halves[i] + halves[j] + svgChannel
				dx := routes[i].labelX - routes[j].labelX
				if dx < 0 {
					dx = -dx
				}
				if dx < gap {
					routes[i].labelX = routes[j].labelX + gap
					moved = true
				}
			}
		}
	}
}

// SVG renders the run view as standalone SVG with a hand-rolled layout:
// longest-path layering top to bottom, nodes spread horizontally inside
// a row, polyline edges with arrowheads. Edges that skip rows or point
// back are routed through free side channels instead of crossing node
// rows, and captions that would overlap are pushed apart. Paint comes
// from the embedding page: currentColor and the --pip-* variables of
// the embedded style cover both light and dark themes. Every node group
// carries class "node node-<kind> state-<state>" for CSS styling.
// Output is deterministic. A zero view renders the graph with all
// states pending.
func SVG(v RunView) string {
	g := v.Graph
	rows := layers(g)

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

	l := &svgLayout{
		boxW:     boxW,
		pos:      make(map[string]point, len(g.Nodes)),
		rowOf:    make(map[string]int, len(g.Nodes)),
		rects:    make(map[string]svgRect, len(g.Nodes)),
		occupied: map[float64]int{},
	}
	// Центры узлов: строки сверху вниз, внутри строки — слева направо.
	y := svgMargin + svgBoxHeight/2
	for ri, row := range rows {
		x := svgMargin + boxW/2
		for _, id := range row {
			l.pos[id] = point{x, y}
			l.rowOf[id] = ri
			l.rects[id] = svgRect{x: x - boxW/2, y: y - svgBoxHeight/2, w: boxW, h: svgBoxHeight}
			l.order = append(l.order, id)
			x += boxW + svgGapX
		}
		y += svgBoxHeight + svgGapY
	}
	width := 2*svgMargin + boxW
	height := 2 * svgMargin
	if n := len(rows); n > 0 {
		height = 2*svgMargin + float64(n)*svgBoxHeight + float64(n-1)*svgGapY
	}

	// Рёбра в порядке сортировки DOT (From, To): вывод не зависит от
	// входного порядка рёбер; состояние берётся по индексу в g.Edges.
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
	routes := make([]svgRoute, 0, len(ordered))
	for _, index := range ordered {
		e := g.Edges[index]
		st, ok := v.Edges[index]
		if !ok {
			st = StatePending
		}
		if r, ok := l.route(e, st); ok {
			routes = append(routes, r)
		}
	}
	separateLabels(routes)

	// Границы содержимого: узлы, маршруты, наконечники и подписи. Узел
	// держит svgMargin со всех сторон: иначе самый правый столбец широкой
	// строки упирается в край, и контур фокуса встроенного графа (#214)
	// обрезается краем прокрутки.
	minX, minY, maxX, maxY := 0.0, 0.0, width, height
	for id := range l.rects {
		r := l.rects[id]
		minX = min(minX, r.x-svgMargin)
		maxX = max(maxX, r.x+r.w+svgMargin)
	}
	for _, r := range routes {
		for _, p := range append(append([]point{}, r.pts...), r.tip) {
			minX = min(minX, p[0]-svgChannel)
			maxX = max(maxX, p[0]+svgChannel)
			minY = min(minY, p[1]-svgChannel)
			maxY = max(maxY, p[1]+svgChannel)
		}
		if r.hasLabel {
			half := halfCaption(r.caption)
			minX = min(minX, r.labelX-half-svgMargin/2)
			maxX = max(maxX, r.labelX+half+svgMargin/2)
			minY = min(minY, r.labelY-svgMargin/2)
			maxY = max(maxY, r.labelY+svgMargin/2)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="%s %s %s %s" role="img">`+"\n", num(minX), num(minY), num(maxX-minX), num(maxY-minY))
	b.WriteString(pipStyle)
	// Рёбра первыми: линии лежат под узлами и подписями.
	for _, r := range routes {
		var pts strings.Builder
		for i, p := range r.pts {
			if i > 0 {
				pts.WriteByte(' ')
			}
			fmt.Fprintf(&pts, "%s,%s", num(p[0]), num(p[1]))
		}
		fmt.Fprintf(&b, `<polyline class="edge edge-%s" points="%s"/>`+"\n", classToken(string(r.st)), pts.String())
		fmt.Fprintf(&b, `<polygon class="edge-arrow edge-%s" points="%s"/>`+"\n", classToken(string(r.st)), arrowPoints(r))
		if r.hasLabel {
			fmt.Fprintf(&b, `<text class="edge-label" x="%s" y="%s" text-anchor="middle">%s</text>`+"\n", num(r.labelX), num(r.labelY), escape(r.caption))
		}
	}
	for _, id := range l.order {
		p := l.pos[id]
		rx := "0"
		if kind[id] == KindStart {
			rx = "20"
		}
		fmt.Fprintf(&b, `<g class="node node-%s state-%s" data-node-id="%s"><rect x="%s" y="%s" width="%s" height="%s" rx="%s"/><text x="%s" y="%s" text-anchor="middle">%s</text></g>`+"\n",
			classToken(string(kind[id])), classToken(string(state[id])), escape(id),
			num(p[0]-boxW/2), num(p[1]-svgBoxHeight/2), num(boxW), num(svgBoxHeight), rx,
			num(p[0]), num(p[1]+5), escape(label[id]))
	}
	b.WriteString("</svg>\n")
	return b.String()
}

// arrowPoints строит наконечник по последнему сегменту маршрута:
// направление входа в цель задаёт ориентацию треугольника.
func arrowPoints(r svgRoute) string {
	n := len(r.pts)
	if n < 2 {
		return fmt.Sprintf("%s,%s %s,%s %s,%s", num(r.tip[0]), num(r.tip[1]),
			num(r.tip[0]-svgArrowSize/2), num(r.tip[1]-svgArrowSize/2),
			num(r.tip[0]+svgArrowSize/2), num(r.tip[1]-svgArrowSize/2))
	}
	prev := r.pts[n-1]
	switch {
	case prev[0] == r.tip[0] && r.tip[1] > prev[1]: // вход сверху вниз
		return fmt.Sprintf("%s,%s %s,%s %s,%s", num(r.tip[0]), num(r.tip[1]),
			num(r.tip[0]-svgArrowSize/2), num(r.tip[1]-svgArrowSize),
			num(r.tip[0]+svgArrowSize/2), num(r.tip[1]-svgArrowSize))
	case prev[0] == r.tip[0]: // вход снизу вверх
		return fmt.Sprintf("%s,%s %s,%s %s,%s", num(r.tip[0]), num(r.tip[1]),
			num(r.tip[0]-svgArrowSize/2), num(r.tip[1]+svgArrowSize),
			num(r.tip[0]+svgArrowSize/2), num(r.tip[1]+svgArrowSize))
	case r.tip[0] > prev[0]: // вход слева направо
		return fmt.Sprintf("%s,%s %s,%s %s,%s", num(r.tip[0]), num(r.tip[1]),
			num(r.tip[0]-svgArrowSize), num(r.tip[1]-svgArrowSize/2),
			num(r.tip[0]-svgArrowSize), num(r.tip[1]+svgArrowSize/2))
	default: // вход справа налево
		return fmt.Sprintf("%s,%s %s,%s %s,%s", num(r.tip[0]), num(r.tip[1]),
			num(r.tip[0]+svgArrowSize), num(r.tip[1]-svgArrowSize/2),
			num(r.tip[0]+svgArrowSize), num(r.tip[1]+svgArrowSize/2))
	}
}
