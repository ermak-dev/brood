// Симуляция прогона: обход графа с фиктивным исполнителем.
package pipeline

import (
	"fmt"
)

// simulateMaxVisits — предохранитель циклов: узел, посещённый чаще,
// означает бесконечный цикл, а не «длинный» конвейер.
const simulateMaxVisits = 50

// Step — один шаг симуляции: узел и выбранное из него ребро.
type Step struct {
	Node string
	Next string
	Edge *Edge
}

// Simulate обходит граф от start: обычный узел считается успешным
// (outcome=success), human-узел выбирает первую опцию и её исход, условие
// context.<имя>=<значение> считается выполненным. Возвращает
// последовательность шагов до exit. Отсутствие подходящего ребра
// или превышение лимита посещений — отказ строкой с причиной.
func (d *Document) Simulate() ([]Step, error) {
	visited := map[string]int{}
	steps := []Step{}
	current := d.Start
	for {
		visited[current]++
		if visited[current] > simulateMaxVisits {
			return nil, fmt.Errorf(
				"pipeline: cycle detected: node %q visited more than %d times without reaching exit %q",
				current, simulateMaxVisits, d.Exit)
		}
		if current == d.Exit {
			return steps, nil
		}
		outcome := "success"
		preferred := ""
		if node, ok := d.Nodes[current]; ok && node.Type == "human" && len(node.Options) > 0 {
			outcome = node.Options[0].Outcome
			preferred = node.Options[0].Label
		}
		// Оптимистичный контекст: сравнение на равенство считается
		// истинным, на неравенство — ложным, без разбора значений.
		edge, ok := selectOptimistic(d.outgoing(current), outcome, preferred)
		if !ok {
			return nil, fmt.Errorf(
				"pipeline: node %q has no outgoing edge matching outcome %q", current, outcome)
		}
		steps = append(steps, Step{Node: current, Next: edge.To, Edge: &edge})
		current = edge.To
	}
}

// selectOptimistic повторяет приоритеты SelectNext, но условия
// context.<имя> трактует оптимистично: совпало — берём первое,
// несовпавшее условие не выбирается никогда.
func selectOptimistic(edges []Edge, outcome, preferred string) (Edge, bool) {
	for _, edge := range edges {
		if edge.Condition == "" {
			continue
		}
		clauses, err := parseCondition(edge.Condition)
		if err != nil {
			continue
		}
		matched := true
		for _, cl := range clauses {
			if cl.key == "outcome" || cl.key == "preferred_label" {
				if !clauseMatches(cl, outcome, preferred, nil) {
					matched = false
				}
			} else if !optimisticContext(cl) {
				matched = false
			}
			if !matched {
				break
			}
		}
		if matched {
			return edge, true
		}
	}
	// Дальше те же приоритеты 2-5, что у SelectNext, с пустым
	// контекстом и без suggested.
	return SelectNext(edges, outcome, map[string]string{}, preferred, nil)
}

// optimisticContext трактует условие контекста оптимистично: равенство
// истинно, неравенство ложно.
func optimisticContext(cl clause) bool {
	return cl.op == "="
}

// PlanLines возвращает текстовое представление симуляции для CLI:
// по строке на шаг и финальную строку выхода.
func (d *Document) PlanLines() ([]string, error) {
	steps, err := d.Simulate()
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(steps)+1)
	for _, step := range steps {
		lines = append(lines, fmt.Sprintf("%s -> %s", step.Node, step.Next))
	}
	if len(steps) > 0 {
		lines = append(lines, fmt.Sprintf("exit at %s", steps[len(steps)-1].Next))
	}
	return lines, nil
}
