package piprender

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update перегенерирует golden-файлы: go test ./piprender -update.
var update = flag.Bool("update", false, "rewrite golden files in testdata")

// sampleGraph покрывает все виды узлов: старт, актор, условие, land,
// человек, parallel, fan_in и exit.
func sampleGraph() Graph {
	return Graph{
		Nodes: []Node{
			{ID: "start", Kind: KindStart, Label: "start"},
			{ID: "build", Kind: KindActor, Label: "build"},
			{ID: "check", Kind: KindCondition, Label: "ci ok?"},
			{ID: "test", Kind: KindActor, Label: "test"},
			{ID: "land", Kind: KindLand, Label: "land"},
			{ID: "review", Kind: KindHuman, Label: "review"},
			{ID: "fanout", Kind: KindParallel, Label: "fan out"},
			{ID: "lint", Kind: KindActor, Label: "lint"},
			{ID: "vet", Kind: KindActor, Label: "vet"},
			{ID: "fanin", Kind: KindFanIn, Label: "fan in"},
			{ID: "exit", Kind: KindExit, Label: "exit"},
		},
		Edges: []Edge{
			{From: "start", To: "build"},
			{From: "build", To: "check"},
			{From: "check", To: "test", Label: "yes"},
			{From: "check", To: "land", Label: "no", Weight: 3},
			{From: "test", To: "review", Condition: "tests pass"},
			{From: "review", To: "fanout"},
			{From: "fanout", To: "lint"},
			{From: "fanout", To: "vet", Weight: 2},
			{From: "lint", To: "fanin"},
			{From: "vet", To: "fanin"},
			{From: "fanin", To: "exit"},
		},
	}
}

func golden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from golden:\n--- golden ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

func TestGoldenDOT(t *testing.T) {
	golden(t, "sample.dot", DOT(sampleGraph()))
}

func TestGoldenSVG(t *testing.T) {
	v := Overlay(sampleGraph(), []string{"start", "build"}, "check", nil)
	golden(t, "sample.svg", SVG(v))
}

func TestDeterministic(t *testing.T) {
	// Один и тот же граф, перемешанный на месте, даёт побайтно тот же
	// вывод; повторный рендер тоже.
	g := sampleGraph()
	firstDot, firstSVG := DOT(g), SVG(Overlay(g, []string{"start"}, "build", nil))
	g.Nodes[1], g.Nodes[9] = g.Nodes[9], g.Nodes[1]
	g.Edges[0], g.Edges[7] = g.Edges[7], g.Edges[0]
	if DOT(g) != firstDot || SVG(Overlay(g, []string{"start"}, "build", nil)) != firstSVG {
		t.Fatal("rendering depends on input order")
	}
	if DOT(g) != DOT(g) {
		t.Fatal("DOT is not deterministic")
	}
}
