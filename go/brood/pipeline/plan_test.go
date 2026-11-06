// Проверки симуляции прогона: обход графа с фиктивным исполнителем.
package pipeline

import (
	"strings"
	"testing"
)

// BUG: симуляция игнорирует исход первой опции человека.
// HYPOTHESIS: исход всех узлов безусловно задан как success.
// SOLUTION: использовать исход первой опции человеческого узла.
// RESULT: FIXED — выбор ребра учитывает исход опции.
// STATUS: FIXED
func TestSimulateHumanOptionOutcome(t *testing.T) {
	d := &Document{Start: "human", Exit: "exit", Nodes: map[string]Node{
		"human": {Type: "human", Options: []Option{{Label: "no", Outcome: "fail"}}},
	}, Edges: []Edge{
		{From: "human", To: "wrong", Condition: "outcome=success"},
		{From: "human", To: "exit", Condition: "outcome=fail"},
	}}
	steps, err := d.Simulate()
	if err != nil || len(steps) != 1 || steps[0].Next != "exit" {
		t.Fatalf("want human -> exit, got %+v, %v", steps, err)
	}
}

// BUG: симуляция неверно сравнивает нормализованные подписи в условиях.
// HYPOTHESIS: симулятор сравнивает подписи буквально вместо использования селектора.
// SOLUTION: применять сравнение селектора к условиям без контекста.
// RESULT: FIXED — нормализованные подписи выбирают одинаковое ребро.
// STATUS: FIXED
func TestSimulatePreferredLabelNormalization(t *testing.T) {
	d := &Document{Start: "human", Exit: "exit", Nodes: map[string]Node{
		"human": {Type: "human", Options: []Option{{Label: "[Y] Yes", Outcome: "success"}}},
	}, Edges: []Edge{
		{From: "human", To: "wrong", Condition: "preferred_label!=yes"},
		{From: "human", To: "exit", Condition: "preferred_label=yes"},
	}}
	steps, err := d.Simulate()
	if err != nil || len(steps) != 1 || steps[0].Next != "exit" {
		t.Fatalf("want normalized label to select exit, got %+v, %v", steps, err)
	}
}

func TestSimulateExampleWalksToExit(t *testing.T) {
	document := validDocument(t)
	steps, err := document.Simulate()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, step := range steps {
		names = append(names, step.Node)
	}
	if len(steps) > 0 {
		names = append(names, steps[len(steps)-1].Next)
	}
	joined := strings.Join(names, " ")
	if !strings.HasPrefix(joined, "prepare") {
		t.Fatalf("walk must start at start node: %v", names)
	}
	if !strings.HasSuffix(joined, "done") {
		t.Fatalf("walk must end at exit node: %v", names)
	}
	// Первый опция человеческого узла — success, поэтому цикл
	// context.report!=ok не выбирается: check идёт в land.
	sawLand := false
	for _, step := range steps {
		if step.Node == "land" {
			sawLand = true
		}
		if step.Node == "check" && step.Next == "land" {
			if step.Edge == nil || step.Edge.Condition != "context.report=ok" {
				t.Fatalf("expected conditioned edge to land: %+v", step)
			}
		}
	}
	if !sawLand {
		t.Fatalf("walk must reach land goal gate: %v", names)
	}
}

func TestSimulateDetectsInfiniteLoop(t *testing.T) {
	text := readExample(t, "testdata/pipeline.yaml")
	// Самозацикливание build с наибольшим весом: рёбра к check остаются,
	// граф структурно валиден, но симуляция никогда не покинет узел.
	text = strings.Replace(text, "  - {from: build, to: check}\n", "  - {from: build, to: check}\n  - {from: build, to: build, weight: 10}\n", 1)
	document, err := Parse(text, "loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("graph is structurally valid: %v", err)
	}
	_, err = document.Simulate()
	if err == nil {
		t.Fatal("must detect the loop")
	}
	if !strings.Contains(err.Error(), "cycle") && !strings.Contains(err.Error(), "loop") {
		t.Fatalf("error must name the cycle: %q", err.Error())
	}
}
