// Золотая проверка DOT-вывода эталонного документа.
package pipeline

import (
	"strings"
	"testing"

	"brood.dev/go/brood/piprender"
)

// BUG: идентификаторы узлов с дефисом или ключевым словом ломают DOT.
// HYPOTHESIS: имена узлов и концы рёбер выводятся без кавычек.
// SOLUTION: заключать идентификаторы в кавычки.
// RESULT: FIXED — идентификаторы не разбивают синтаксис DOT.
// STATUS: FIXED
func TestRenderDotQuotesIdentifiers(t *testing.T) {
	d := validDocument(t)
	d.Nodes["node-with-hyphens"] = d.Nodes["prepare"]
	delete(d.Nodes, "prepare")
	d.Start = "node-with-hyphens"
	d.Edges[0].From = d.Start
	text, err := d.RenderDot()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"node-with-hyphens" [`, `"node-with-hyphens" -> review`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in %s", want, text)
		}
	}
}

func TestRenderDotGolden(t *testing.T) {
	document := validDocument(t)
	text, err := document.RenderDot()
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Join([]string{
		`digraph pipeline {`,
		`  rankdir=LR;`,
		`  build [label="build", shape=box];`,
		`  check [label="check", shape=diamond];`,
		`  done [label="done", shape=Msquare];`,
		`  land [label="land", shape=house];`,
		`  prepare [label="prepare", shape=Mdiamond];`,
		`  review [label="review", shape=hexagon];`,
		`  build -> check;`,
		`  check -> land [label="context.report=ok"];`,
		`  check -> review [label="context.report!=ok w=1"];`,
		`  land -> done;`,
		`  prepare -> review;`,
		`  review -> build [label="outcome=success"];`,
		`  review -> done [label="outcome=fail"];`,
		`}`, ``,
	}, "\n")
	if text != expected {
		t.Fatalf("DOT mismatch:\ngot:\n%s\nwant:\n%s", text, expected)
	}
}

func TestRenderDotEscapesQuotes(t *testing.T) {
	document := validDocument(t)
	// Подпись ребра с кавычками должна попасть в DOT экранированной.
	document.Edges[0].Label = `say "hi"`
	text, err := document.RenderDot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `[label="say \"hi\""]`) {
		t.Fatalf("labels must escape quotes: %s", text)
	}
}

func TestRenderDotShapeMapping(t *testing.T) {
	text := `apiVersion: brood.dev/v1alpha1
name: shapes
start: s
exit: e
nodes:
  s:
    type: actor
    template: example
    prompt: go
  h:
    type: human
    question: q
    options:
      - {label: "[Y] yes", outcome: success}
  c:
    type: condition
  l:
    type: land
    target: main
  e:
    type: exit
edges:
  - {from: s, to: h}
  - {from: h, to: c, condition: "outcome=success"}
  - {from: c, to: l}
  - {from: l, to: e}
goalGates: [l]
`
	document, err := Parse(text, "shapes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dot, err := document.RenderDot()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		// Узел, названный в start, получает Mdiamond вместо формы типа.
		`shape=Mdiamond`, `shape=hexagon`, `shape=diamond`, `shape=house`, `shape=Msquare`,
	} {
		if !strings.Contains(dot, expected) {
			t.Fatalf("missing %s in:\n%s", expected, dot)
		}
	}
}

// FEATURE: Adapt pipeline documents to the shared renderer.
// SPEC: Start and exit use designated IDs; edge captions and weights survive, while prompts stay private.
// IMPLEMENTATION: Both render entry points consume the same graph.
// RESULT: IMPLEMENTED — graph structure is shared without node payloads.
// STATUS: DONE
func TestGraphAdapter(t *testing.T) {
	d := validDocument(t)
	d.Nodes[d.Start] = Node{Type: "actor", Template: "example", Prompt: "secret payload"}
	d.Edges[0].Label = "a & b"
	d.Edges[0].Weight = 7
	g := d.Graph()
	dot, err := d.RenderDot()
	if err != nil {
		t.Fatal(err)
	}
	if dot != piprender.DOT(g) {
		t.Fatal("DOT does not use adapted graph")
	}
	svg := piprender.SVG(piprender.RunView{Graph: g})
	for _, s := range []string{"node-start", "node-exit", "a &amp; b w=7"} {
		if !strings.Contains(svg, s) {
			t.Errorf("missing %q", s)
		}
	}
	if strings.Contains(svg, "secret payload") || strings.Contains(dot, "secret payload") {
		t.Fatal("node payload leaked")
	}
	if ((*Document)(nil)).Graph().Nodes != nil {
		t.Fatal("nil document graph is not empty")
	}
}
