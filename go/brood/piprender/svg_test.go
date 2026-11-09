package piprender

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// nodeX достаёт x-координату текста узла с данным label.
var textX = regexp.MustCompile(`<text x="([0-9.]+)" y="[0-9.]+" text-anchor="middle">`)

func xOfLabel(t *testing.T, svg, label string) float64 {
	t.Helper()
	i := strings.Index(svg, `>`+label+`</text>`)
	if i < 0 {
		t.Fatalf("label %s missing:\n%s", label, svg)
	}
	match := textX.FindStringSubmatch(svg[clampLo(i-160) : i+1])
	if match == nil {
		t.Fatalf("text x of %s not found:\n%s", label, svg)
	}
	x, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		t.Fatalf("parse x of %s: %v", label, err)
	}
	return x
}

// nodeY достаёт y-координату текста узла с данным label.
var textY = regexp.MustCompile(`<text x="[-0-9.]+" y="([0-9.]+)" text-anchor="middle">`)

func yOfLabel(t *testing.T, svg, label string) float64 {
	t.Helper()
	i := strings.Index(svg, `>`+label+`</text>`)
	if i < 0 {
		t.Fatalf("label %s missing:\n%s", label, svg)
	}
	match := textY.FindStringSubmatch(svg[clampLo(i-160) : i+1])
	if match == nil {
		t.Fatalf("text y of %s not found:\n%s", label, svg)
	}
	y, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		t.Fatalf("parse y of %s: %v", label, err)
	}
	return y
}

// clampLo не даёт окну поиска уйти в отрицательные индексы.
func clampLo(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

func TestSVGLayering(t *testing.T) {
	// Линейная цепочка: каждый узел строго ниже предыдущего.
	svg := SVG(RunView{Graph: chain()})
	ya, yb := yOfLabel(t, svg, "a"), yOfLabel(t, svg, "b")
	yc, yd := yOfLabel(t, svg, "c"), yOfLabel(t, svg, "d")
	if !(ya < yb && yb < yc && yc < yd) {
		t.Fatalf("nodes not layered in chain order: a=%v b=%v c=%v d=%v\n%s", ya, yb, yc, yd, svg)
	}
	// Ветвление fan out: общие преемники дальше обоих источников.
	svg = SVG(RunView{Graph: sampleGraph()})
	if !(yOfLabel(t, svg, "lint") > yOfLabel(t, svg, "fan out") && yOfLabel(t, svg, "fan in") > yOfLabel(t, svg, "lint")) {
		t.Fatalf("fan-out layout broken:\n%s", svg)
	}
}

func TestSVGClassesAndEscape(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "a", Kind: KindHuman, Label: `Tom & <Jerry>`}},
	}
	svg := SVG(Overlay(g, nil, "a", nil))
	if !strings.Contains(svg, `<g class="node node-human state-active">`) {
		t.Fatalf("node class missing:\n%s", svg)
	}
	if !strings.Contains(svg, ">Tom &amp; &lt;Jerry&gt;</text>") {
		t.Fatalf("label not XML-escaped:\n%s", svg)
	}
	svg = SVG(Overlay(g, nil, "", []string{"a"}))
	if !strings.Contains(svg, `<g class="node node-human state-failed">`) {
		t.Fatalf("failed state class missing:\n%s", svg)
	}
	// Нулевой view без состояний: pending по умолчанию.
	svg = SVG(RunView{Graph: g})
	if !strings.Contains(svg, `<g class="node node-human state-pending">`) {
		t.Fatalf("pending default state missing:\n%s", svg)
	}
}

func TestSVGEdgeStates(t *testing.T) {
	v := Overlay(chain(), []string{"a", "b"}, "c", nil)
	svg := SVG(v)
	for _, want := range []string{
		`class="edge edge-done"`, `class="edge edge-active"`, `class="edge edge-pending"`,
		`<polygon class="edge-arrow edge-active"`,
	} {
		if !strings.Contains(svg, want) {
			t.Fatalf("%s missing:\n%s", want, svg)
		}
	}
}

func TestSVGViewBoxSizedToContent(t *testing.T) {
	svg := SVG(RunView{Graph: sampleGraph()})
	i := strings.Index(svg, `viewBox="`)
	end := strings.Index(svg[i+9:], `"`)
	parts := strings.Fields(svg[i+9 : i+9+end])
	if len(parts) != 4 {
		t.Fatalf("viewBox must have four numbers: %q", svg[i:i+60])
	}
	nums := make([]float64, 4)
	for j, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil {
			t.Fatalf("viewBox number %q: %v", p, err)
		}
		nums[j] = n
	}
	// Восемь слоёв sampleGraph: высота заметно больше ширины одной
	// строки; самая широкая строка из двух узлов задаёт ширину.
	if nums[3] < 500 {
		t.Fatalf("height suspiciously small: %v", nums[3])
	}
	if nums[2] < 100 {
		t.Fatalf("width suspiciously small: %v", nums[2])
	}
	// Максимальная координата содержимого не выходит за viewBox.
	for _, m := range regexp.MustCompile(`(x|y)="([0-9.]+)"`).FindAllStringSubmatch(svg, -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		limit := nums[3]
		if m[1] == "x" {
			limit = nums[2]
		}
		// points-полилиния уже покрыта двумя пробелами внутри viewBox;
		// отдельные атрибуты не должны вылезать за пределы.
		if v > limit+1 {
			t.Fatalf("coordinate %s=%v exceeds viewBox %v", m[1], v, limit)
		}
	}
}

// BUG: Identical edges lose distinct overlay states.
// HYPOTHESIS: Value-keyed lookup replaces the first edge index.
// SOLUTION: Sort original indices and use each index for its overlay.
// RESULT: FIXED — identical edges retain distinct states.
// STATUS: FIXED
func TestSVGIdenticalEdgeStates(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "a"}, {ID: "b"}}, Edges: []Edge{{From: "a", To: "b"}, {From: "a", To: "b"}}}
	v := RunView{Graph: g, Edges: map[int]State{0: StateDone, 1: StateActive}}
	s := SVG(v)
	if !strings.Contains(s, `class="edge edge-done"`) || !strings.Contains(s, `class="edge edge-active"`) {
		t.Fatalf("distinct edge states missing: %s", s)
	}
	if s != SVG(v) {
		t.Fatal("output is not deterministic")
	}
}

// BUG: Long edge captions extend beyond the viewBox.
// HYPOTHESIS: Bounds account for nodes but not caption width.
// SOLUTION: Include estimated caption and routed edge bounds in the viewBox.
// RESULT: FIXED — estimated ASCII and Unicode caption bounds fit.
// STATUS: FIXED
func TestSVGCaptionBounds(t *testing.T) {
	for _, caption := range []string{strings.Repeat("wide", 80), strings.Repeat("界", 200) + " & <"} {
		g := Graph{Nodes: []Node{{ID: "a"}, {ID: "b"}}, Edges: []Edge{{From: "a", To: "b", Label: caption}}}
		s := SVG(RunView{Graph: g})
		var doc struct {
			ViewBox string `xml:"viewBox,attr"`
			Text    struct {
				X     float64 `xml:"x,attr"`
				Value string  `xml:",chardata"`
			} `xml:"text"`
		}
		if err := xml.Unmarshal([]byte(s), &doc); err != nil {
			t.Fatal(err)
		}
		var left, top, width, height float64
		if _, err := fmt.Sscan(doc.ViewBox, &left, &top, &width, &height); err != nil {
			t.Fatal(err)
		}
		half := float64(utf8.RuneCountInString(caption)) * svgGlyphWidth * 12 / 16 / 2
		if doc.Text.Value != caption {
			t.Fatalf("caption lost or escaped incorrectly: %q", doc.Text.Value)
		}
		if doc.Text.X-half < left || doc.Text.X+half > left+width {
			t.Errorf("caption bounds [%v,%v] outside viewBox [%v,%v]", doc.Text.X-half, doc.Text.X+half, left, left+width)
		}
	}
}

func TestSVGEmptyGraph(t *testing.T) {
	svg := SVG(RunView{})
	if !strings.HasPrefix(svg, `<svg `) || !strings.HasSuffix(strings.TrimSpace(svg), "</svg>") {
		t.Fatalf("empty graph must render a closed svg:\n%s", svg)
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("empty graph produced NaN coordinates:\n%s", svg)
	}
}

// BUG: SVG defaults hid edges and labels; cycles collapsed and classes permitted XML injection.
// HYPOTHESIS: presentation attributes and class escaping are missing; relaxation diverges on cycles.
// SOLUTION: an embedded style drives standalone paint, classes are escaped, cycles break deterministically.
// RESULT: FIXED — standalone elements have paint via the style block and cycles have separate layers.
// STATUS: FIXED
func TestSVGRegression(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "a", Label: "a", Kind: Kind(`x" onload="evil`)}, {ID: "b", Label: "b"}}, Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}}}
	svg := SVG(RunView{Graph: g, Nodes: map[string]State{"a": State(`x" onclick="evil`)}, Edges: map[int]State{0: State(`x" onclick="evil`)}})
	dec := xml.NewDecoder(strings.NewReader(svg))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid XML: %v", err)
		}
		if e, ok := tok.(xml.StartElement); ok {
			for _, a := range e.Attr {
				if a.Name.Local == "onload" || a.Name.Local == "onclick" {
					t.Fatalf("injected attribute: %v", a)
				}
			}
		}
	}
	if yOfLabel(t, svg, "a") == yOfLabel(t, svg, "b") {
		t.Error("cycle nodes collapse into one layer")
	}
	// Зашитых красок в разметке нет: стиль задаёт currentColor.
	if strings.Contains(svg, `fill="white"`) || strings.Contains(svg, `stroke="black"`) || strings.Contains(svg, `fill="black"`) {
		t.Error("hardcoded color attribute remains in markup")
	}
	if !strings.Contains(svg, "currentColor") {
		t.Error("standalone paint must use currentColor")
	}
}

func TestSVGCycleBounded(t *testing.T) {
	// Цикл не вешает раскладку: валидатор интеграции откажет раньше,
	// но рендер остаётся конечным и детерминированным.
	g := Graph{
		Nodes: []Node{{ID: "a", Kind: KindActor}, {ID: "b", Kind: KindActor}},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}
	first := SVG(RunView{Graph: g})
	if first != SVG(RunView{Graph: g}) {
		t.Fatal("cycle layout is not deterministic")
	}
	if strings.Contains(first, "NaN") {
		t.Fatalf("cycle produced NaN:\n%s", first)
	}
}

// FEATURE: Preserve transition captions in SVG.
// SPEC: Condition wins over label; captions are XML escaped, including on cyclic edges.
// IMPLEMENTATION: Render escaped captions for forward and backward edges.
// RESULT: IMPLEMENTED — captions survive both routes.
// STATUS: DONE
func TestSVGTransitionCaptions(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "a", Label: "a"}, {ID: "b", Label: "b"}}, Edges: []Edge{{From: "a", To: "b", Label: "ignored", Condition: "x < y", Weight: 2}, {From: "b", To: "a", Label: "return & retry"}}}
	out := SVG(RunView{Graph: g})
	for _, want := range []string{"x &lt; y w=2", "return &amp; retry"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

// FEATURE: Lay out cyclic workflows from their designated start.
// SPEC: An alphabetically earlier cycle must not precede the start node.
// IMPLEMENTATION: Traverse designated starts before other nodes.
// RESULT: IMPLEMENTED — start anchors reachable cycles.
// STATUS: DONE
func TestSVGStartBeforeCycle(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "z", Kind: KindStart}, {ID: "a"}, {ID: "b"}}, Edges: []Edge{{From: "z", To: "b"}, {From: "a", To: "b"}, {From: "b", To: "a"}}}
	cols := layers(g)
	if len(cols) != 3 || len(cols[0]) != 1 || cols[0][0] != "z" {
		t.Fatalf("unexpected layers: %v", cols)
	}
}

// FEATURE: Escape untrusted graph text.
// SPEC: DOT strings cannot contain literal newlines and SVG must be valid XML.
// IMPLEMENTATION: Escape line breaks and replace invalid XML characters.
// RESULT: IMPLEMENTED — untrusted labels cannot break SVG syntax.
// STATUS: DONE
func TestGraphEscapingControlCharacters(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "a", Label: "line\nnext\x01"}}}
	dot := DOT(g)
	if strings.Contains(dot, "line\nnext") {
		t.Errorf("literal newline in DOT label: %q", dot)
	}
	svg := SVG(RunView{Graph: g})
	var v any
	if err := xml.Unmarshal([]byte(svg), &v); err != nil {
		t.Fatalf("invalid SVG: %v", err)
	}
}
