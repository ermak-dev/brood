package piprender

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestDOTNodeOrder(t *testing.T) {
	g := Graph{
		Nodes: []Node{
			{ID: "zeta", Kind: KindActor, Label: "z", Order: 3},
			{ID: "alpha", Kind: KindActor, Label: "a", Order: 1},
			{ID: "mid", Kind: KindActor, Label: "m", Order: 2},
			{ID: "aa", Kind: KindActor, Label: "aa"},
		},
		Edges: []Edge{{From: "alpha", To: "zeta"}, {From: "zeta", To: "alpha"}},
	}
	dot := DOT(g)
	iZeta, iAlpha, iMid := strings.Index(dot, "zeta"), strings.Index(dot, "alpha"), strings.Index(dot, "mid")
	if !(iAlpha < iMid && iMid < iZeta) {
		t.Fatalf("explicit Order must be ascending: alpha < mid < zeta:\n%s", dot)
	}
	// Узел без явного Order (0) стоит раньше назначенных.
	if !(strings.Index(dot, "aa") < iAlpha) {
		t.Fatalf("zero Order must sort before assigned orders:\n%s", dot)
	}
	// Рёбра сортированы по (From, To): alpha->zeta раньше zeta->alpha.
	iEdgeAZ := strings.Index(dot, "alpha -> zeta")
	iEdgeZA := strings.Index(dot, "zeta -> alpha")
	if !(0 < iEdgeAZ && iEdgeAZ < iEdgeZA) {
		t.Fatalf("edges not sorted by (from, to):\n%s", dot)
	}
}

func TestDOTShapes(t *testing.T) {
	for kind, shape := range map[Kind]string{
		KindStart: "Mdiamond", KindExit: "Msquare", KindActor: "box",
		KindHuman: "hexagon", KindCondition: "diamond", KindLand: "house",
		KindParallel: "component", KindFanIn: "tripleoctagon",
	} {
		dot := DOT(Graph{Nodes: []Node{{ID: "n", Kind: kind, Label: "n"}}})
		if !strings.Contains(dot, "shape="+shape) {
			t.Fatalf("kind %s: shape %s missing:\n%s", kind, shape, dot)
		}
	}
}

func TestDOTEdgeLabels(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "a", Kind: KindStart}, {ID: "b", Kind: KindExit}},
		Edges: []Edge{
			{From: "a", To: "b", Label: "yes"},
			{From: "a", To: "b", Condition: "ci ok", Label: "ignored"},
			{From: "b", To: "a", Label: "w", Weight: 2},
			{From: "b", To: "a", Weight: 4},
		},
	}
	dot := DOT(g)
	for _, want := range []string{
		`[label="yes"]`, `[label="ci ok"]`, `[label="w w=2"]`, `[label="w=4"]`,
	} {
		if !strings.Contains(dot, want) {
			t.Fatalf("edge label %s missing:\n%s", want, dot)
		}
	}
}

func TestDOTQuoting(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "a b", Kind: KindActor, Label: `say "hi" <&>`}},
		Edges: []Edge{{From: "a b", To: "a b", Label: `x\y`}},
	}
	dot := DOT(g)
	for _, want := range []string{
		`"a b" [`, `"say \"hi\" <&>"`, `shape=box`, `"a b" -> "a b" [label="x\\y"]`,
	} {
		if !strings.Contains(dot, want) {
			t.Fatalf("%s missing:\n%s", want, dot)
		}
	}
	if strings.Contains(dot, "a b [") {
		t.Fatalf("unquoted spaced id leaked:\n%s", dot)
	}
}

// BUG: DOT keywords and IDs beginning with digits are emitted as bare identifiers.
// HYPOTHESIS: the identifier check accepts digits in the first position and reserved words.
// SOLUTION: quote reserved words and identifiers that cannot be bare DOT IDs.
// RESULT: FIXED — these identifiers no longer change DOT syntax.
// STATUS: FIXED
func TestDOTIdentifierRegression(t *testing.T) {
	for _, id := range []string{"graph", "123abc", "node"} {
		got := DOT(Graph{Nodes: []Node{{ID: id}}})
		if !strings.Contains(got, `"`+id+`" [`) {
			t.Errorf("unsafe identifier %q: %s", id, got)
		}
	}
}

// BUG: identifiers ending in a backslash produce invalid DOT.
// HYPOTHESIS: quoting escapes quotes but leaves terminal backslashes unescaped.
// SOLUTION: escape backslashes as well as quotes in identifiers.
// RESULT: FIXED — Graphviz parses distinct nodes and their edges.
// STATUS: FIXED
func TestDOTBackslashIdentifiersGraphviz(t *testing.T) {
	if _, err := exec.LookPath("dot"); err != nil {
		t.Skip("Graphviz dot is not installed")
	}
	g := Graph{
		Nodes: []Node{{ID: `end\`, Label: "first"}, {ID: `end\\`, Label: "second"}, {ID: `a\"b`, Label: "third"}},
		Edges: []Edge{{From: `end\`, To: `end\\`}, {From: `end\\`, To: `a\"b`}},
	}
	input := DOT(g)
	cmd := exec.Command("dot", "-Tjson")
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Graphviz rejected DOT: %v\n%s\n%s", err, output, input)
	}
	var parsed struct {
		Objects []struct {
			Label string `json:"label"`
		} `json:"objects"`
		Edges []struct{} `json:"edges"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	for _, node := range parsed.Objects {
		labels[node.Label] = true
	}
	if len(parsed.Objects) != 3 || len(parsed.Edges) != 2 || !labels["first"] || !labels["second"] || !labels["third"] {
		t.Fatalf("Graphviz changed nodes, labels, or edges: %s", output)
	}
}

func TestDOTUnknownKindFallsBackToBox(t *testing.T) {
	dot := DOT(Graph{Nodes: []Node{{ID: "n", Kind: Kind("weird"), Label: "n"}}})
	if !strings.Contains(dot, "shape=box") {
		t.Fatalf("unknown kind must fall back to box:\n%s", dot)
	}
}
