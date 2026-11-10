package pipeline_test

// FEATURE: Вход в стартовый узел нового прохода (#211).
// SPEC: Begin — чистая функция: первое посещение прохода — стартовый узел;
// actor-узел ждёт доставки (pending, прогон running), human-узел ждёт
// оператора (waiting, прогон waiting_human) — то же правило, по которому
// Advance входит в преемника. Граф, который исполнитель не умеет вести,
// отвергается до решения.
// RESULT: IMPLEMENTED — запуск роя берёт первый шаг отсюда.
// STATUS: DONE

import (
	"testing"

	"brood.dev/go/brood/pipeline"
)

// actorStartGraph — проход начинается с актора.
const actorStartGraph = `apiVersion: brood.dev/v1alpha1
name: actor-start
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: build, to: done}
goalGates: [done]
`

func TestBeginEntersStartNode(t *testing.T) {
	human := parseEngine(t, "engine", engineGraph)
	actor := parseEngine(t, "actor-start", actorStartGraph)
	for _, tc := range []struct {
		name string
		doc  *pipeline.Document
		want pipeline.Transition
	}{
		{"human start", human, pipeline.Transition{Next: "ask", NextState: "waiting", Execution: "waiting_human"}},
		{"actor start", actor, pipeline.Transition{Next: "build", NextState: "pending", Execution: "running"}},
	} {
		got, err := pipeline.Begin(tc.doc)
		if err != nil || got != tc.want {
			t.Fatalf("%s: Begin = %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
}

// TestBeginMatchesAdvanceEntry — вход в узел при запуске и при переходе
// по ребру один и тот же: Begin не заводит своего правила состояний.
func TestBeginMatchesAdvanceEntry(t *testing.T) {
	d := parseEngine(t, "engine", engineGraph)
	viaEdge, err := pipeline.Advance(d, pipeline.Event{Node: "ask", State: "answered", Outcome: "success"})
	if err != nil {
		t.Fatal(err)
	}
	d.Start = "actor"
	begun, err := pipeline.Begin(d)
	if err != nil {
		t.Fatal(err)
	}
	if begun.NextState != viaEdge.NextState || begun.Execution != viaEdge.Execution {
		t.Fatalf("Begin entry %+v differs from Advance entry %+v", begun, viaEdge)
	}
}

// TestBeginRefusesUnsupported — Begin, как и Advance, не решает за граф,
// который исполнитель вести не умеет, и не входит в узел без исполнения.
func TestBeginRefusesUnsupported(t *testing.T) {
	retries := parseEngine(t, "retries", engineGraph+"retry: {maxRetries: 2}\n")
	if _, err := pipeline.Begin(retries); err == nil {
		t.Fatal("Begin accepted a graph with retries")
	}
	if _, err := pipeline.Begin(nil); err == nil {
		t.Fatal("Begin accepted no document")
	}
	missing := parseEngine(t, "engine", engineGraph)
	missing.Start = "absent"
	if _, err := pipeline.Begin(missing); err == nil {
		t.Fatal("Begin entered an undefined start node")
	}
	exit := parseEngine(t, "engine", engineGraph)
	exit.Start = "done"
	if _, err := pipeline.Begin(exit); err == nil {
		t.Fatal("Begin entered the exit as the first visit")
	}
}
