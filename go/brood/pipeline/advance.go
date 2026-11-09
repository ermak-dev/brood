// Машина переходов конвейера: чистая функция от документа, шага и события.
// Порт решающей части progressPipeline из хранилища (#207): выбор ребра,
// состояние следующей вершины и статус прогона вычисляются без ввода-вывода;
// хранилище применяет результат и ничего не решает.
package pipeline

import (
	"errors"
	"strings"
)

// Event is one resolved visit outcome: the node, its recorded state and the
// answer of a human node or the outcome of an actor node.
type Event struct {
	Node  string
	State string // "answered" for a human visit, "completed" for an actor visit
	// Outcome is the operator answer of a human node or the actor outcome.
	Outcome string
}

// Transition is the applied result of one Event. Execution is the new run
// status: "running", "waiting_human", "succeeded" or "failed". Terminal
// marks a fail-closed stop without a successor visit: the run has no edge
// for the outcome and ends failed at the current node.
type Transition struct {
	Next      string
	NextState string
	Execution string
	Terminal  bool
}

// Supported rejects execution semantics this runner cannot honor: retries,
// non-exit goal gates, managed pools, node types it never dispatches,
// conditions beyond outcome comparisons and human options without an edge.
// The store runs it before accepting a graph; Advance runs it again so a
// decision is never made for a document the runner could not execute.
func Supported(d *Document) error {
	if d.Retry != nil && d.Retry.MaxRetries != 0 {
		return errors.New("pipeline retries are unsupported")
	}
	for _, gate := range d.GoalGates {
		if gate != d.Exit {
			return errors.New("pipeline goal gates are unsupported")
		}
	}
	for id, n := range d.Nodes {
		if n.Type == "actor" && n.ManagedPool != "" {
			return errors.New("managed pool pipeline execution is unsupported")
		}
		if n.Type != "actor" && n.Type != "human" && n.Type != "exit" {
			return errors.New("pipeline node type is unsupported")
		}
		if n.Type == "exit" && id != d.Exit {
			return errors.New("pipeline has a non-designated exit")
		}
		if n.Type == "human" {
			seen := map[string]bool{}
			for _, option := range n.Options {
				if seen[option.Outcome] {
					return errors.New("ambiguous pipeline human outcome")
				}
				seen[option.Outcome] = true
				if _, ok := selectEdge(d, id, "answered", option.Outcome); !ok {
					return errors.New("pipeline human option has no matching edge")
				}
			}
		}
	}
	for _, e := range d.Edges {
		if e.Condition != "" {
			for _, clause := range strings.Split(e.Condition, "&&") {
				if !strings.HasPrefix(strings.TrimSpace(clause), "outcome=") && !strings.HasPrefix(strings.TrimSpace(clause), "outcome!=") {
					return errors.New("pipeline condition is unsupported")
				}
			}
		}
	}
	return nil
}

// selectEdge выбирает ребро узла по состоянию и значению. Подпись варианта
// human-узла сужает выбор: помеченные ребра с чужой подписью не участвуют.
// Отказ актёра не идёт по безусловному ребру: безусловное ребро означает
// «в любом случае», а не «при отказе».
func selectEdge(d *Document, node, state, value string) (Edge, bool) {
	preferred := ""
	if state == "answered" {
		for _, option := range d.Nodes[node].Options {
			if option.Outcome == value {
				preferred = option.Label
				break
			}
		}
	}
	var edges []Edge
	for _, edge := range d.Edges {
		if edge.From != node || (state == "completed" && value == "fail" && edge.Condition == "") {
			continue
		}
		if state == "answered" && edge.Label != "" && edge.Label != preferred {
			continue
		}
		edges = append(edges, edge)
	}
	return SelectNext(edges, value, nil, preferred, nil)
}

// Advance computes the transition for one event without side effects.
// A completed actor visit carries outcome "success" or "fail"; an answered
// human visit carries one of the node's option outcomes. When no edge
// matches, the run stops failed at the current node (Terminal). Edge
// targets must be the designated exit by name; anything else is an error.
func Advance(d *Document, e Event) (Transition, error) {
	if err := Supported(d); err != nil {
		return Transition{}, err
	}
	node, ok := d.Nodes[e.Node]
	if !ok {
		return Transition{}, errors.New("pipeline node not found")
	}
	switch {
	case e.State == "answered" && node.Type != "human":
		return Transition{}, errors.New("pipeline visit is not a human node")
	case e.State == "completed" && node.Type != "actor":
		return Transition{}, errors.New("pipeline visit is not an actor node")
	case e.State != "answered" && e.State != "completed":
		return Transition{}, errors.New("pipeline visit state is unsupported")
	}
	if e.Outcome == "" {
		return Transition{}, errors.New("pipeline event outcome is empty")
	}
	if node.Type == "human" {
		known := false
		for _, option := range node.Options {
			if option.Outcome == e.Outcome {
				known = true
			}
		}
		if !known {
			return Transition{}, errors.New("pipeline outcome does not match any human option")
		}
	} else if e.Outcome != "success" && e.Outcome != "fail" {
		return Transition{}, errors.New("pipeline outcome is unsupported")
	}
	edge, ok := selectEdge(d, e.Node, e.State, e.Outcome)
	if !ok {
		// Отказ без подходящего ребра — терминальный отказ: шаг записан,
		// преемника нет, прогон завершён.
		return Transition{Execution: "failed", Terminal: true}, nil
	}
	if d.Nodes[edge.To].Type == "exit" && edge.To != d.Exit {
		return Transition{}, errors.New("pipeline has a non-designated exit")
	}
	next := d.Nodes[edge.To]
	result := Transition{Next: edge.To, Execution: "running"}
	switch next.Type {
	case "human":
		result.NextState = "waiting"
		result.Execution = "waiting_human"
	case "exit":
		result.NextState = "completed"
		result.Execution = "succeeded"
		if e.Outcome == "fail" {
			result.Execution = "failed"
		}
	case "actor":
		result.NextState = "pending"
	default:
		return Transition{}, errors.New("pipeline node type is unsupported")
	}
	return result, nil
}
