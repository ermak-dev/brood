// Package pipeline implements the declarative pipeline document schema
// for the pipeline runner (epic #165): strict parsing, graph validation,
// edge selection, walk simulation and DOT rendering.
//
// Схема brood.dev/v1alpha1: узлы (actor, human, condition, land, exit,
// parallel, fan_in), рёбра с condition/weight/label, goalGates и retry.
// Actor-узлы называют роль, а не способ создания актора (#208): связывает
// роль с актором рой, провижининг остаётся в шаблонах и пулах. Разбор
// строгий: неизвестные поля, якоря и повторяющиеся ключи отвергаются,
// отказ — строка с причиной.
package pipeline

import (
	"fmt"
	"os"
	"regexp"
	"sort"

	"brood.dev/go/brood/workflowpolicy"
	"gopkg.in/yaml.v3"
)

// nodeTypes — закрытый список типов узлов.
var nodeTypes = map[string]bool{
	"actor": true, "human": true, "condition": true, "land": true,
	"exit": true, "parallel": true, "fan_in": true,
}

var patternDNSName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Option — вариант ответа человеческого узла.
type Option struct {
	Label   string `yaml:"label"`
	Outcome string `yaml:"outcome"`
}

// Node — узел конвейера с типом и полями своего типа. Role обязательна
// у actor-узлов: имя, по которому рой связывает узел с существующим актором.
type Node struct {
	Type          string           `yaml:"type"`
	Role          string           `yaml:"role,omitempty"`
	Prompt        string           `yaml:"prompt,omitempty"`
	Question      string           `yaml:"question,omitempty"`
	Options       []Option         `yaml:"options,omitempty"`
	Target        string           `yaml:"target,omitempty"`
	Branches      []string         `yaml:"branches,omitempty"`
	ChildJoin     *ChildJoinPolicy `yaml:"childJoin,omitempty"`
	ActionClasses []string         `yaml:"actionClasses,omitempty"`
}

// ChildJoinPolicy defines an actor node's server-computed child outcome.
// Omitted mode means all. Total membership is fixed by the first child intent;
// quorum is a node-defined threshold, not a guest-selected value.
type ChildJoinPolicy struct {
	Mode   string `yaml:"mode,omitempty"`
	Quorum int    `yaml:"quorum,omitempty"`
}

// ChildJoinMode returns the node's join mode, defaulting to all.
func (n Node) ChildJoinMode() string {
	if n.ChildJoin == nil || n.ChildJoin.Mode == "" {
		return "all"
	}
	return n.ChildJoin.Mode
}

// Edge — ребро графа; пустое условие всегда истинно.
type Edge struct {
	From      string `yaml:"from"`
	To        string `yaml:"to"`
	Condition string `yaml:"condition,omitempty"`
	Label     string `yaml:"label,omitempty"`
	Weight    int    `yaml:"weight,omitempty"`
}

// Retry — политика повторов неудачных узлов.
type Retry struct {
	MaxRetries int `yaml:"maxRetries"`
}

// Document — разобранный документ конвейера.
type Document struct {
	APIVersion string          `yaml:"apiVersion"`
	Name       string          `yaml:"name"`
	Start      string          `yaml:"start"`
	Exit       string          `yaml:"exit"`
	Nodes      map[string]Node `yaml:"nodes"`
	Edges      []Edge          `yaml:"edges"`
	GoalGates  []string        `yaml:"goalGates"`
	Retry      *Retry          `yaml:"retry"`
}

// Severity — уровень диагностики; ERROR блокирует прогон.
type Severity string

const (
	ERROR   Severity = "ERROR"
	WARNING Severity = "WARNING"
	INFO    Severity = "INFO"
)

// Diagnostic — одна строка диагностики валидатора.
type Diagnostic struct {
	Severity Severity
	Message  string
}

// ParseFile читает файл и разбирает документ.
func ParseFile(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pipeline: read %s: %w", path, err)
	}
	return Parse(string(data), path)
}

// Parse строго разбирает текст документа: один YAML-документ без якорей
// и повторов ключей, только известные поля, точный apiVersion.
func Parse(text, filename string) (*Document, error) {
	return parse(text, filename, true)
}

// ParseEmbedded validates a workflow already decoded from a size-limited document.
// The generated YAML is not itself subject to the input size limit.
func ParseEmbedded(value map[string]any, filename string) (*Document, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return parse(string(data), filename, false)
}

func parse(text, filename string, limitSize bool) (*Document, error) {
	document, err := decodeDocument(text, filename, limitSize)
	if err != nil {
		return nil, err
	}
	if err := document.checkSchema(filename); err != nil {
		return nil, err
	}
	return document, nil
}

// checkSchema проверяет значения полей и условия рёбер.
func (d *Document) checkSchema(filename string) error {
	fail := func(field, reason string) error {
		return fmt.Errorf("%s: %s: %s", filename, field, reason)
	}
	if !patternDNSName.MatchString(d.Name) {
		return fail("name", "must be a lowercase DNS label")
	}
	for name, node := range d.Nodes {
		if !nodeTypes[node.Type] {
			return fail("nodes."+name+".type",
				fmt.Sprintf("unknown node type %q; expected one of actor, human, condition, land, exit, parallel, fan_in", node.Type))
		}
		if node.Role != "" && (node.Type != "actor" || !patternDNSName.MatchString(node.Role)) {
			return fail("nodes."+name+".role", "must be a lowercase DNS label on an actor node")
		}
		if node.Type == "actor" && node.Role == "" {
			return fail("nodes."+name+".role", "actor node requires a role; role replaced template, managedPool and styles")
		}
		if node.ActionClasses != nil {
			if node.Type != "actor" || len(node.ActionClasses) == 0 || len(node.ActionClasses) > 6 {
				return fail("nodes."+name+".actionClasses", "requires an actor and one to six explicit classes")
			}
			seen := map[string]bool{}
			for _, action := range node.ActionClasses {
				if !workflowpolicy.ValidAction(action) || seen[action] || action == "work" && len(node.ActionClasses) != 1 {
					return fail("nodes."+name+".actionClasses", "classes must be known, unique and work must stand alone")
				}
				seen[action] = true
			}
		}
		if node.ChildJoin != nil {
			if node.Type != "actor" {
				return fail("nodes."+name+".childJoin", "only actor nodes support child joins")
			}
			switch node.ChildJoinMode() {
			case "all", "any":
				if node.ChildJoin.Quorum != 0 {
					return fail("nodes."+name+".childJoin.quorum", "requires quorum mode")
				}
			case "quorum":
				if node.ChildJoin.Quorum < 1 || node.ChildJoin.Quorum > 64 {
					return fail("nodes."+name+".childJoin.quorum", "must be between 1 and 64")
				}
			default:
				return fail("nodes."+name+".childJoin.mode", "must be all, any or quorum")
			}
		}
		if node.Type == "human" {
			if len(node.Options) == 0 {
				return fail("nodes."+name+".options", "human node requires at least one option")
			}
			for index, option := range node.Options {
				if option.Label == "" {
					return fail(fmt.Sprintf("nodes.%s.options.%d.label", name, index), "option label must not be empty")
				}
				if option.Outcome == "" {
					return fail(fmt.Sprintf("nodes.%s.options.%d.outcome", name, index), "option outcome must not be empty")
				}
			}
		}
	}
	for index, edge := range d.Edges {
		if edge.From == "" || edge.To == "" {
			return fail(fmt.Sprintf("edges.%d", index), "edge requires from and to")
		}
		if edge.Condition != "" {
			if _, err := parseCondition(edge.Condition); err != nil {
				return fail(fmt.Sprintf("edges.%d.condition", index), err.Error())
			}
		}
	}
	if d.Retry != nil && d.Retry.MaxRetries < 0 {
		return fail("retry.maxRetries", "must be zero or greater")
	}
	return nil
}

// Roles returns the set of distinct role names used by actor nodes.
// The set is a derived value: roles exist only as node fields.
func (d *Document) Roles() map[string]bool {
	result := map[string]bool{}
	for _, node := range d.Nodes {
		if node.Type == "actor" {
			result[node.Role] = true
		}
	}
	return result
}

// outgoing возвращает рёбра узла в порядке документа.
func (d *Document) outgoing(node string) []Edge {
	result := make([]Edge, 0, 2)
	for _, edge := range d.Edges {
		if edge.From == node {
			result = append(result, edge)
		}
	}
	return result
}

// Validate проверяет граф: уникальность start/exit, существование
// концов рёбер, достижимость всех узлов и выхода, goalGates.
func (d *Document) Validate() error {
	if _, ok := d.Nodes[d.Start]; !ok {
		return fmt.Errorf("pipeline: start node %q is not defined in nodes", d.Start)
	}
	if _, ok := d.Nodes[d.Exit]; !ok {
		return fmt.Errorf("pipeline: exit node %q is not defined in nodes", d.Exit)
	}
	if errors := d.nodeGraphErrors(); len(errors) > 0 {
		return fmt.Errorf("pipeline: %s", errors[0])
	}
	for index, edge := range d.Edges {
		if _, ok := d.Nodes[edge.From]; !ok {
			return fmt.Errorf("pipeline: edges[%d] references undefined node %q in from", index, edge.From)
		}
		if _, ok := d.Nodes[edge.To]; !ok {
			return fmt.Errorf("pipeline: edges[%d] references undefined node %q in to", index, edge.To)
		}
	}
	if len(d.outgoing(d.Exit)) > 0 {
		return fmt.Errorf("pipeline: exit node %q must not have outgoing edges", d.Exit)
	}
	for index, edge := range d.Edges {
		if edge.To == d.Start {
			return fmt.Errorf("pipeline: start node %q must not have incoming edges (edges[%d])", d.Start, index)
		}
	}
	// Достижимость всех узлов от start.
	forward, reverse := graphAdjacency(d.Edges)
	reachable := graphReachable(forward, d.Start)
	for _, name := range d.sortedNodeNames() {
		if !reachable[name] {
			return fmt.Errorf("pipeline: node %q is not reachable from start %q", name, d.Start)
		}
	}
	// Выход достижим из каждого узла, кроме самого выхода.
	toExit := graphReachable(reverse, d.Exit)
	for _, name := range d.sortedNodeNames() {
		if name == d.Exit || toExit[name] {
			continue
		}
		return fmt.Errorf("pipeline: exit %q is not reachable from node %q", d.Exit, name)
	}
	if len(d.GoalGates) == 0 {
		return fmt.Errorf("pipeline: goalGates must list at least one node")
	}
	for _, gate := range d.GoalGates {
		if gate == d.Start {
			return fmt.Errorf("pipeline: goal gate %q must not be the start node", gate)
		}
		if _, ok := d.Nodes[gate]; !ok {
			return fmt.Errorf("pipeline: goal gate %q is not defined in nodes", gate)
		}
	}
	return nil
}

// nodeGraphErrors проверяет ограничения узлов, общие для обеих форм валидации.
// Форму роли проверяет и checkSchema при разборе; здесь она повторена для
// документа, собранного без разбора.
func (d *Document) nodeGraphErrors() []string {
	var errors []string
	for _, name := range d.sortedNodeNames() {
		node := d.Nodes[name]
		if node.Role != "" && (node.Type != "actor" || !patternDNSName.MatchString(node.Role)) {
			errors = append(errors, fmt.Sprintf("node %q role must be a lowercase DNS label on an actor", name))
		}
		if node.Type == "actor" && node.Role == "" {
			errors = append(errors, fmt.Sprintf("actor node %q requires a role", name))
		}
		if node.Type == "exit" && name != d.Exit {
			errors = append(errors, fmt.Sprintf("exit node %q is not the designated exit %q", name, d.Exit))
		}
		if node.Type == "parallel" {
			for _, branch := range node.Branches {
				if _, ok := d.Nodes[branch]; !ok {
					errors = append(errors, fmt.Sprintf("parallel node %q references undefined branch %q", name, branch))
				}
			}
		}
		if node.Type == "human" {
			seen := map[string]bool{}
			for _, option := range node.Options {
				if seen[option.Outcome] {
					errors = append(errors, fmt.Sprintf("human node %q has duplicate option outcome %q", name, option.Outcome))
				}
				seen[option.Outcome] = true
			}
		}
	}
	return errors
}

// sortedNodeNames — узлы в стабильном порядке для воспроизводимых отказов.
func (d *Document) sortedNodeNames() []string {
	names := make([]string, 0, len(d.Nodes))
	for name := range d.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// graphAdjacency строит оба направления, включая ссылки на неизвестные узлы.
func graphAdjacency(edges []Edge) (map[string][]string, map[string][]string) {
	forward := make(map[string][]string)
	reverse := make(map[string][]string)
	for _, edge := range edges {
		forward[edge.From] = append(forward[edge.From], edge.To)
		reverse[edge.To] = append(reverse[edge.To], edge.From)
	}
	return forward, reverse
}

// graphReachable обходит каждую достижимую вершину и её рёбра один раз.
func graphReachable(adjacency map[string][]string, start string) map[string]bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[current] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}

// Diagnostics возвращает ошибки валидации, предупреждения и сведения;
// ошибки соответствуют отказу Validate, но не прерывают обход.
func (d *Document) Diagnostics() ([]Diagnostic, error) {
	result := []Diagnostic{}
	appendf := func(severity Severity, format string, args ...any) {
		result = append(result, Diagnostic{severity, fmt.Sprintf(format, args...)})
	}
	if err := d.checkSchema("document"); err != nil {
		return nil, err
	}
	// Структурные ошибки собираем как ERROR-диагностики, не прерывая.
	for _, message := range d.nodeGraphErrors() {
		appendf(ERROR, "%s", message)
	}
	if _, ok := d.Nodes[d.Start]; !ok {
		appendf(ERROR, "start node %q is not defined in nodes", d.Start)
	}
	if _, ok := d.Nodes[d.Exit]; !ok {
		appendf(ERROR, "exit node %q is not defined in nodes", d.Exit)
	}
	for index, edge := range d.Edges {
		if _, ok := d.Nodes[edge.From]; !ok {
			appendf(ERROR, "edges[%d] references undefined node %q in from", index, edge.From)
		}
		if _, ok := d.Nodes[edge.To]; !ok {
			appendf(ERROR, "edges[%d] references undefined node %q in to", index, edge.To)
		}
	}
	if len(d.outgoing(d.Exit)) > 0 {
		appendf(ERROR, "exit node %q must not have outgoing edges", d.Exit)
	}
	for _, edge := range d.Edges {
		if edge.To == d.Start {
			appendf(ERROR, "start node %q must not have incoming edges", d.Start)
			break
		}
	}
	if len(d.Nodes) > 0 {
		if _, ok := d.Nodes[d.Start]; ok {
			forward, reverse := graphAdjacency(d.Edges)
			reachable := graphReachable(forward, d.Start)
			for _, name := range d.sortedNodeNames() {
				if !reachable[name] {
					appendf(ERROR, "node %q is not reachable from start %q", name, d.Start)
				}
			}
			if _, ok := d.Nodes[d.Exit]; ok {
				toExit := graphReachable(reverse, d.Exit)
				for _, name := range d.sortedNodeNames() {
					if name != d.Exit && !toExit[name] {
						appendf(ERROR, "exit %q is not reachable from node %q", d.Exit, name)
					}
				}
			}
		}
	}
	for _, gate := range d.GoalGates {
		if gate == d.Start {
			appendf(ERROR, "goal gate %q must not be the start node", gate)
		}
		if _, ok := d.Nodes[gate]; !ok {
			appendf(ERROR, "goal gate %q is not defined in nodes", gate)
		}
	}
	if len(d.GoalGates) == 0 {
		appendf(ERROR, "goalGates must list at least one node")
	}
	// Предупреждения.
	for _, edge := range d.Edges {
		if edge.Condition != "" && edge.Weight != 0 {
			appendf(WARNING, "edge %s -> %s combines a condition with weight; condition already decides the match",
				edge.From, edge.To)
		}
	}
	return result, nil
}
