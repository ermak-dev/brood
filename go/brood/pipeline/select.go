// Грамматика условий рёбер и чистый выбор следующего ребра.
package pipeline

import (
	"fmt"
	"strings"
)

// clause — один сравнительный терм условия: key op value.
type clause struct {
	key   string
	op    string // "=" или "!="
	value string
}

// parseCondition разбирает условие: термы, соединённые "&&"; ключи
// outcome, preferred_label и context.<имя>; операторы "=" и "!=";
// значение непустое. Пустая строка — допустимое тривиальное условие,
// его разбирать не нужно.
func parseCondition(text string) ([]clause, error) {
	parts := strings.Split(text, "&&")
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("condition clause must not be empty")
	}
	var result []clause
	for index, part := range parts {
		if strings.TrimSpace(part) == "" {
			if index == 0 {
				return nil, fmt.Errorf("condition clause %d must not be empty", index)
			}
			return nil, fmt.Errorf("condition %q: clause %d must not be empty (check for a trailing or doubled &&)", text, index)
		}
		cl, err := parseClause(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("condition %q: clause %d: %v", text, index, err)
		}
		result = append(result, cl)
	}
	return result, nil
}

// parseClause разбирает один терм key op value.
func parseClause(part string) (clause, error) {
	var cl clause
	eq := strings.Index(part, "=")
	neq := strings.Index(part, "!=")
	// Ищем первую позицию оператора: "!=" встречается раньше "=".
	pos, op := -1, ""
	if neq >= 0 && (eq < 0 || neq <= eq-1) {
		pos, op = neq, "!="
	} else if eq >= 0 {
		pos, op = eq, "="
	}
	if pos < 0 {
		return cl, fmt.Errorf("expected key=value or key!=value")
	}
	key := strings.TrimSpace(part[:pos])
	value := strings.TrimSpace(part[pos+len(op):])
	if op == "=" && strings.HasPrefix(value, "=") {
		return cl, fmt.Errorf("unsupported operator (expected = or !=)")
	}
	if !validKey(key) {
		return cl, fmt.Errorf("unknown condition key %q (expected outcome, preferred_label or context.<name>)", key)
	}
	if value == "" {
		return cl, fmt.Errorf("condition value must not be empty")
	}
	return clause{key: key, op: op, value: value}, nil
}

// validKey проверяет имя ключа условия.
func validKey(key string) bool {
	if key == "outcome" || key == "preferred_label" {
		return true
	}
	if name, ok := strings.CutPrefix(key, "context."); ok {
		return name != ""
	}
	return false
}

// resolveValue подставляет значение ключа условия: outcome и
// preferred_label — из аргументов, context.<имя> — из словаря;
// отсутствующий ключ контекста даёт пустую строку.
func resolveValue(key, outcome, preferred string, context map[string]string) string {
	switch key {
	case "outcome":
		return outcome
	case "preferred_label":
		return preferred
	}
	if name, ok := strings.CutPrefix(key, "context."); ok {
		return context[name]
	}
	return ""
}

// clauseMatches сравнивает терм с текущими значениями. Значение
// preferred_label сравнивается после нормализации обеих сторон.
func clauseMatches(cl clause, outcome, preferred string, context map[string]string) bool {
	actual := resolveValue(cl.key, outcome, preferred, context)
	if cl.key == "preferred_label" {
		if cl.op == "=" {
			return normalizeLabel(actual) == normalizeLabel(cl.value)
		}
		return normalizeLabel(actual) != normalizeLabel(cl.value)
	}
	if cl.op == "=" {
		return actual == cl.value
	}
	return actual != cl.value
}

// normalizeLabel приводит подпись к сравнимому виду: нижний регистр,
// без префикса-акселератора вида "[Y] " и без крайних пробелов.
func normalizeLabel(label string) string {
	trimmed := strings.TrimSpace(label)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "[") {
		if end := strings.Index(lower, "]"); end >= 0 {
			return strings.TrimSpace(lower[end+1:])
		}
	}
	return lower
}

// SelectNext выбирает следующее ребро из данных рёбер одного узла
// по приоритету: (1) первое условие, все термы которого совпали,
// (2) ребро с нормализованной подписью preferred, (3) первый target
// из suggested, (4) наибольший weight, (5) лексикографически меньший
// target. ok=false, когда ни одно ребро не подходит.
func SelectNext(edges []Edge, outcome string, context map[string]string, preferred string, suggested []string) (Edge, bool) {
	// Приоритет 1: условия в порядке документа.
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
			if !clauseMatches(cl, outcome, preferred, context) {
				matched = false
				break
			}
		}
		if matched {
			return edge, true
		}
	}
	// Приоритет 2: подпись, совпавшая после нормализации.
	if preferred != "" {
		wanted := normalizeLabel(preferred)
		for _, edge := range edges {
			if edge.Condition == "" && edge.Label != "" && normalizeLabel(edge.Label) == wanted {
				return edge, true
			}
		}
	}
	// Приоритет 3: первый предложенный target, существующий среди рёбер.
	for _, id := range suggested {
		if id == "" {
			continue
		}
		for _, edge := range edges {
			if edge.Condition == "" && edge.To == id {
				return edge, true
			}
		}
	}
	// Приоритеты 4-5: среди рёбер без условия — наибольший weight,
	// при равенстве — меньший target. Несовпавшие условные рёбра
	// сюда не попадают: их условие не выполнено.
	var best Edge
	found := false
	for _, edge := range edges {
		if edge.Condition != "" {
			continue
		}
		if !found {
			best, found = edge, true
			continue
		}
		switch {
		case edge.Weight > best.Weight:
			best = edge
		case edge.Weight == best.Weight && edge.To < best.To:
			best = edge
		}
	}
	return best, found
}
