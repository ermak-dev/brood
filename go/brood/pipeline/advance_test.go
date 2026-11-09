// Правила машины переходов конвейера: чистая функция от документа, шага
// и события, без хранилища. Порт решающей части progressPipeline (#207).
package pipeline_test

import (
	"os"
	"strings"
	"testing"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/pipeline"
)

// engineGraph — базовый граф: человек с двумя подписанными рёбрами,
// актёр с условным ребром успеха.
const engineGraph = `apiVersion: brood.dev/v1alpha1
name: engine
start: ask
exit: done
nodes:
  ask:
    type: human
    options:
      - {label: Yes, outcome: success}
      - {label: No, outcome: fail}
  actor: {type: actor, template: development, prompt: work}
  done: {type: exit}
edges:
  - {from: ask, to: actor, label: Yes}
  - {from: ask, to: done, label: No}
  - {from: actor, to: done, condition: "outcome=success"}
goalGates: [done]
`

// parseEngine разбирает и валидирует граф или падает с именем теста.
func parseEngine(t *testing.T, name, text string) *pipeline.Document {
	t.Helper()
	d, err := pipeline.Parse(text, name)
	if err != nil {
		t.Fatalf("%s: parse: %v", name, err)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("%s: validate: %v", name, err)
	}
	return d
}

// mustEdit заменяет в тексте графа from на to ровно один раз.
func mustEdit(t *testing.T, name, text, from, to string) string {
	t.Helper()
	if n := strings.Count(text, from); n != 1 {
		t.Fatalf("%s: edit anchor %q occurs %d times", name, from, n)
	}
	return strings.Replace(text, from, to, 1)
}

// TestAdvanceRoutes — таблица маршрутов одного события: исход определяет
// следующую вершину, её состояние и статус прогона.
func TestAdvanceRoutes(t *testing.T) {
	d := parseEngine(t, "engine", engineGraph)
	// Человек без подписанных рёбер: успех уходит прямо в выход.
	direct := parseEngine(t, "direct", `apiVersion: brood.dev/v1alpha1
name: direct
start: ask
exit: done
nodes:
  ask:
    type: human
    options:
      - {label: Ship, outcome: success}
  done: {type: exit}
edges:
  - {from: ask, to: done}
goalGates: [done]
`)
	// Успех актёра приводит к человеку — прогон ждёт оператора.
	toHuman := parseEngine(t, "to-human", `apiVersion: brood.dev/v1alpha1
name: tohuman
start: work
exit: done
nodes:
  work: {type: actor, template: development, prompt: work}
  ask:
    type: human
    options:
      - {label: Yes, outcome: success}
  done: {type: exit}
edges:
  - {from: work, to: ask}
  - {from: ask, to: done}
goalGates: [done]
`)
	for _, tc := range []struct {
		name string
		g    *pipeline.Document
		e    pipeline.Event
		want pipeline.Transition
	}{
		{
			name: "answer success follows label",
			g:    d,
			e:    pipeline.Event{Node: "ask", State: "answered", Outcome: "success"},
			want: pipeline.Transition{Next: "actor", NextState: "pending", Execution: "running"},
		},
		{
			name: "answer fail follows label into exit and fails",
			g:    d,
			e:    pipeline.Event{Node: "ask", State: "answered", Outcome: "fail"},
			want: pipeline.Transition{Next: "done", NextState: "completed", Execution: "failed"},
		},
		{
			name: "actor success continues by condition",
			g:    d,
			e:    pipeline.Event{Node: "actor", State: "completed", Outcome: "success"},
			want: pipeline.Transition{Next: "done", NextState: "completed", Execution: "succeeded"},
		},
		{
			name: "actor fail without matching edge terminates",
			g:    d,
			e:    pipeline.Event{Node: "actor", State: "completed", Outcome: "fail"},
			want: pipeline.Transition{Terminal: true, Execution: "failed"},
		},
		{
			name: "unlabeled human answer reaches exit",
			g:    direct,
			e:    pipeline.Event{Node: "ask", State: "answered", Outcome: "success"},
			want: pipeline.Transition{Next: "done", NextState: "completed", Execution: "succeeded"},
		},
		{
			name: "actor success into human waits",
			g:    toHuman,
			e:    pipeline.Event{Node: "work", State: "completed", Outcome: "success"},
			want: pipeline.Transition{Next: "ask", NextState: "waiting", Execution: "waiting_human"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pipeline.Advance(tc.g, tc.e)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Advance: got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAdvanceFailClosedRecovery — отказ актёра с условным ребром
// восстановления уходит в ремонт, а не в терминальный отказ.
func TestAdvanceFailClosedRecovery(t *testing.T) {
	d := parseEngine(t, "recovery", `apiVersion: brood.dev/v1alpha1
name: recovery
start: work
exit: done
nodes:
  work: {type: actor, template: development, prompt: work}
  repair: {type: actor, template: development, prompt: repair}
  done: {type: exit}
edges:
  - {from: work, to: done, condition: "outcome=success"}
  - {from: work, to: repair, condition: "outcome=fail"}
  - {from: repair, to: done}
goalGates: [done]
`)
	got, err := pipeline.Advance(d, pipeline.Event{Node: "work", State: "completed", Outcome: "fail"})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	want := pipeline.Transition{Next: "repair", NextState: "pending", Execution: "running"}
	if got != want {
		t.Fatalf("Advance: got %+v, want %+v", got, want)
	}
}

// TestAdvanceHumanLabelNarrowsEdge — подпись варианта сужает выбор ребра:
// чужое подписанное ребро не может перебить маршрут варианта весом.
func TestAdvanceHumanLabelNarrowsEdge(t *testing.T) {
	d := parseEngine(t, "labels", `apiVersion: brood.dev/v1alpha1
name: labels
start: ask
exit: done
nodes:
  ask:
    type: human
    options:
      - {label: Approve, outcome: success}
  actor: {type: actor, template: development, prompt: work}
  slow: {type: actor, template: development, prompt: slow}
  done: {type: exit}
edges:
  - {from: ask, to: slow, label: Defer, weight: 10}
  - {from: ask, to: actor}
  - {from: actor, to: done}
  - {from: slow, to: done}
goalGates: [done]
`)
	got, err := pipeline.Advance(d, pipeline.Event{Node: "ask", State: "answered", Outcome: "success"})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	want := pipeline.Transition{Next: "actor", NextState: "pending", Execution: "running"}
	if got != want {
		t.Fatalf("label narrowing lost: got %+v, want %+v", got, want)
	}
}

// TestAdvanceRejects — события, которые машина обязана отвергнуть.
func TestAdvanceRejects(t *testing.T) {
	d := parseEngine(t, "engine", engineGraph)
	// Документ с чужим выходом: Validate его отвергнет, поэтому собираем вручную.
	wrongExit := &pipeline.Document{
		APIVersion: "brood.dev/v1alpha1",
		Name:       "wrong-exit",
		Start:      "actor",
		Exit:       "done",
		Nodes: map[string]pipeline.Node{
			"actor": {Type: "actor", Template: "development"},
			"done":  {Type: "exit"},
			"other": {Type: "exit"},
		},
		Edges:     []pipeline.Edge{{From: "actor", To: "other"}},
		GoalGates: []string{"done"},
	}
	for _, tc := range []struct {
		name string
		g    *pipeline.Document
		e    pipeline.Event
	}{
		{"unknown node", d, pipeline.Event{Node: "missing", State: "answered", Outcome: "success"}},
		{"unknown state", d, pipeline.Event{Node: "ask", State: "invented", Outcome: "success"}},
		{"actor event on human node", d, pipeline.Event{Node: "ask", State: "completed", Outcome: "success"}},
		{"human event on actor node", d, pipeline.Event{Node: "actor", State: "answered", Outcome: "success"}},
		{"unknown outcome", d, pipeline.Event{Node: "actor", State: "completed", Outcome: "maybe"}},
		{"exit by wrong name", wrongExit, pipeline.Event{Node: "actor", State: "completed", Outcome: "success"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pipeline.Advance(tc.g, tc.e)
			if err == nil {
				t.Fatalf("Advance accepted %+v: %+v", tc.e, got)
			}
		})
	}
}

// TestSupportedRejects — документы с неисполнимой семантикой отвергаются
// до начала работы.
func TestSupportedRejects(t *testing.T) {
	for _, tc := range []struct {
		name, graph string
	}{
		{"retry", mustEdit(t, "retry", engineGraph, "goalGates: [done]",
			"retry: {maxRetries: 2}\ngoalGates: [done]")},
		{"goal gate", mustEdit(t, "gate", engineGraph, "goalGates: [done]", "goalGates: [actor]")},
		{"managed pool", mustEdit(t, "pool", engineGraph, "template: development, prompt: work",
			"managedPool: pool, prompt: work")},
		{"condition node", mustEdit(t, "condition", engineGraph, "  actor: {type: actor, template: development, prompt: work}",
			"  actor: {type: actor, template: development, prompt: work}\n  check: {type: condition}")},
		{"unsupported condition key", mustEdit(t, "cond", engineGraph,
			`condition: "outcome=success"`, `condition: "context.mode=dev"`)},
		{"human option without matching edge", mustEdit(t, "option", engineGraph,
			`  - {from: ask, to: done, label: No}`, ``)},
		{"ambiguous human outcome", `apiVersion: brood.dev/v1alpha1
name: ambiguous
start: ask
exit: done
nodes:
  ask:
    type: human
    options:
      - {label: Yes, outcome: success}
      - {label: Also, outcome: success}
  done: {type: exit}
edges:
  - {from: ask, to: done}
goalGates: [done]
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := pipeline.Parse(tc.graph, tc.name)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := pipeline.Supported(d); err == nil {
				t.Fatal("Supported accepted an unsupported document")
			}
		})
	}
}

// TestSupportedAccepts — рабочий документ проходит проверку.
func TestSupportedAccepts(t *testing.T) {
	d := parseEngine(t, "engine", engineGraph)
	if err := pipeline.Supported(d); err != nil {
		t.Fatalf("Supported: %v", err)
	}
}

// TestAdvanceRejectsUnsupportedDocument — Advance не даёт успешный переход
// для документа, который Supported обязан отвергнуть.
func TestAdvanceRejectsUnsupportedDocument(t *testing.T) {
	d := parseEngine(t, "engine", mustEdit(t, "cond", engineGraph,
		`condition: "outcome=success"`, `condition: "context.mode=dev"`))
	if _, err := pipeline.Advance(d, pipeline.Event{Node: "actor", State: "completed", Outcome: "success"}); err == nil {
		t.Fatal("Advance succeeded for a document Supported must reject")
	}
}

// TestAdvanceSwarmWorkflowRoutes — маршруты рабочего Broodfile: успех
// проходит цепочку, отказ любого актёра завершает прогон, отказ человека
// на решении уходит в выход.
func TestAdvanceSwarmWorkflowRoutes(t *testing.T) {
	data, err := os.ReadFile("../../../Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := build.ParseDocument(string(data), "Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		chain []pipeline.Event
		// steps ожидает переход по успеху каждой стадии цепочки и
		// терминальный отказ любого актёра; successNext задаёт маршрут
		// каждой стадии, включая прыжок человека на решение.
		successNext map[string]pipeline.Transition
		human       string // имя human-узла или пусто
	}{
		{
			name:  "swarm-development",
			chain: []pipeline.Event{{Node: "triage"}, {Node: "implement"}, {Node: "review"}, {Node: "test"}, {Node: "integrate"}},
			successNext: map[string]pipeline.Transition{
				"triage":    {Next: "implement", NextState: "pending", Execution: "running"},
				"implement": {Next: "review", NextState: "pending", Execution: "running"},
				"review":    {Next: "test", NextState: "pending", Execution: "running"},
				"test":      {Next: "integrate", NextState: "pending", Execution: "running"},
				"integrate": {Next: "done", NextState: "completed", Execution: "succeeded"},
			},
		},
		{
			name:  "swarm-architecture",
			chain: []pipeline.Event{{Node: "triage"}, {Node: "architecture"}, {Node: "implement"}, {Node: "review"}, {Node: "test"}, {Node: "integrate"}},
			successNext: map[string]pipeline.Transition{
				"triage":       {Next: "architecture", NextState: "pending", Execution: "running"},
				"architecture": {Next: "decision", NextState: "waiting", Execution: "waiting_human"},
				"implement":    {Next: "review", NextState: "pending", Execution: "running"},
				"review":       {Next: "test", NextState: "pending", Execution: "running"},
				"test":         {Next: "integrate", NextState: "pending", Execution: "running"},
				"integrate":    {Next: "done", NextState: "completed", Execution: "succeeded"},
			},
			human: "decision",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph, err := build.Workflow(doc, tc.name, "Broodfile")
			if err != nil {
				t.Fatal(err)
			}
			if err := graph.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := pipeline.Supported(graph); err != nil {
				t.Fatal(err)
			}
			// Отказ любого актёра завершает прогон терминально.
			for _, stage := range tc.chain {
				got, err := pipeline.Advance(graph, pipeline.Event{Node: stage.Node, State: "completed", Outcome: "success"})
				if err != nil || got != tc.successNext[stage.Node] {
					t.Fatalf("%s success: got %+v, want %+v: %v", stage.Node, got, tc.successNext[stage.Node], err)
				}
				wantFail := pipeline.Transition{Terminal: true, Execution: "failed"}
				if got, err := pipeline.Advance(graph, pipeline.Event{Node: stage.Node, State: "completed", Outcome: "fail"}); err != nil || got != wantFail {
					t.Fatalf("%s fail: got %+v, want %+v: %v", stage.Node, got, wantFail, err)
				}
			}
			if tc.human != "" {
				approve := pipeline.Transition{Next: "implement", NextState: "pending", Execution: "running"}
				if got, err := pipeline.Advance(graph, pipeline.Event{Node: tc.human, State: "answered", Outcome: "success"}); err != nil || got != approve {
					t.Fatalf("decision approve: got %+v, want %+v: %v", got, approve, err)
				}
				reject := pipeline.Transition{Next: "done", NextState: "completed", Execution: "failed"}
				if got, err := pipeline.Advance(graph, pipeline.Event{Node: tc.human, State: "answered", Outcome: "fail"}); err != nil || got != reject {
					t.Fatalf("decision reject: got %+v, want %+v: %v", got, reject, err)
				}
			}
		})
	}
}
