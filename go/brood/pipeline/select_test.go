// Табличные проверки чистого выбора ребра и грамматики условий.
package pipeline

import (
	"strings"
	"testing"
)

// BUG: предпочтение и предложенные цели выбирают ребро с ложным условием.
// HYPOTHESIS: эти приоритеты не отфильтровывают условные рёбра.
// SOLUTION: при этих приоритетах выбирать только безусловные рёбра.
// RESULT: FIXED — ложное условие не обходится.
// STATUS: FIXED
func TestSelectNextRejectsFalsePreferredAndSuggested(t *testing.T) {
	for _, tc := range []struct {
		preferred string
		suggested []string
	}{
		{preferred: "yes"}, {suggested: []string{"blocked"}},
	} {
		edge, ok := SelectNext([]Edge{{To: "blocked", Label: "yes", Condition: "outcome=fail"}, {To: "allowed"}}, "success", nil, tc.preferred, tc.suggested)
		if !ok || edge.To != "allowed" {
			t.Errorf("preferred=%q suggested=%v: got %+v, %v; want allowed", tc.preferred, tc.suggested, edge, ok)
		}
	}
}

func TestSelectNextTable(t *testing.T) {
	cases := []struct {
		name      string
		edges     []Edge
		outcome   string
		context   map[string]string
		preferred string
		suggested []string
		wantTo    string
		wantOK    bool
	}{
		{
			name: "condition match wins over everything",
			edges: []Edge{
				{From: "review", To: "done", Condition: "outcome=fail"},
				{From: "review", To: "build", Condition: "outcome=success"},
				{From: "review", To: "other", Label: "[Y] yes", Weight: 9},
			},
			outcome: "success",
			wantTo:  "build",
			wantOK:  true,
		},
		{
			name: "first matching condition in document order",
			edges: []Edge{
				{From: "a", To: "first", Condition: "outcome=success"},
				{From: "a", To: "second", Condition: "outcome=success"},
			},
			outcome: "success",
			wantTo:  "first",
			wantOK:  true,
		},
		{
			name: "preferred label match normalizes accelerator prefix and case",
			edges: []Edge{
				{From: "gate", To: "no", Label: "[N] no"},
				{From: "gate", To: "yes", Label: "[Y] Yes"},
			},
			preferred: "[y] YES",
			wantTo:    "yes",
			wantOK:    true,
		},
		{
			name: "suggested id beats weight",
			edges: []Edge{
				{From: "a", To: "heavy", Weight: 5},
				{From: "a", To: "suggested"},
			},
			suggested: []string{"suggested"},
			wantTo:    "suggested",
			wantOK:    true,
		},
		{
			name: "highest weight wins among unconditional edges",
			edges: []Edge{
				{From: "a", To: "low", Weight: 1},
				{From: "a", To: "high", Weight: 2},
				{From: "a", To: "zero"},
			},
			wantTo: "high",
			wantOK: true,
		},
		{
			name: "weight tie resolved by lexicographic target",
			edges: []Edge{
				{From: "a", To: "zeta", Weight: 2},
				{From: "a", To: "alpha", Weight: 2},
			},
			wantTo: "alpha",
			wantOK: true,
		},
		{
			name: "default tie resolved by lexicographic target",
			edges: []Edge{
				{From: "a", To: "b"},
				{From: "a", To: "a2"},
			},
			wantTo: "a2",
			wantOK: true,
		},
		{
			name: "missing context key resolves to empty string",
			edges: []Edge{
				{From: "check", To: "retry", Condition: "context.report!=ok"},
			},
			context: nil,
			wantTo:  "retry",
			wantOK:  true,
		},
		{
			name: "context equality matches value",
			edges: []Edge{
				{From: "check", To: "land", Condition: "context.report=ok"},
				{From: "check", To: "retry", Condition: "context.report!=ok"},
			},
			context: map[string]string{"report": "ok"},
			wantTo:  "land",
			wantOK:  true,
		},
		{
			name: "unmatched conditioned edge is skipped",
			edges: []Edge{
				{From: "a", To: "wrong", Condition: "outcome=fail"},
				{From: "a", To: "plain"},
			},
			outcome: "success",
			wantTo:  "plain",
			wantOK:  true,
		},
		{
			name: "empty condition is unconditional, weight applies",
			edges: []Edge{
				{From: "a", To: "always", Condition: ""},
				{From: "a", To: "weighted", Weight: 9},
			},
			wantTo: "weighted",
			wantOK: true,
		},
		{
			name: "clauses joined by AND must all match",
			edges: []Edge{
				{From: "a", To: "both", Condition: "outcome=success && context.report=ok"},
				{From: "a", To: "fallback"},
			},
			outcome: "success",
			context: map[string]string{"report": "no"},
			wantTo:  "fallback",
			wantOK:  true,
		},
		{
			name: "preferred_label clause compares normalized values",
			edges: []Edge{
				{From: "gate", To: "yes", Condition: "preferred_label=[Y] yes"},
			},
			preferred: "YES",
			wantTo:    "yes",
			wantOK:    true,
		},
		{
			name: "conditioned edge that does not match is skipped, unconditional used",
			edges: []Edge{
				{From: "a", To: "wrong", Condition: "outcome=fail"},
				{From: "a", To: "plain"},
			},
			outcome: "success",
			wantTo:  "plain",
			wantOK:  true,
		},
		{
			name: "no edges at all",
			edges: []Edge{
				{From: "a", To: "x", Condition: "outcome=fail"},
			},
			outcome: "success",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edge, ok := SelectNext(tc.edges, tc.outcome, tc.context, tc.preferred, tc.suggested)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
			if ok && edge.To != tc.wantTo {
				t.Fatalf("target=%q want %q", edge.To, tc.wantTo)
			}
		})
	}
}

func TestSelectNextRejectsEdgesFromOtherNodes(t *testing.T) {
	// Выбор учитывает только рёбра текущего узла: вызывающий код фильтрует
	// сам, но функция обязана не путать порядок приоритетов при смешении.
	edges := []Edge{
		{From: "other", To: "far", Condition: "outcome=success"},
		{From: "here", To: "near"},
	}
	edge, ok := SelectNext(edges[1:], "success", nil, "", nil)
	if !ok || edge.To != "near" {
		t.Fatalf("edge=%+v ok=%v", edge, ok)
	}
}

func TestParseConditionGrammar(t *testing.T) {
	valid := []string{
		"outcome=success",
		"outcome != fail",
		"preferred_label=[Y] да",
		"context.report=ok",
		"context.my_key!=value with spaces",
		"outcome=success && context.report=ok && preferred_label=x",
	}
	for _, cond := range valid {
		if _, err := parseCondition(cond); err != nil {
			t.Errorf("condition %q must parse: %v", cond, err)
		}
	}
	invalid := map[string]string{
		"outcome=":                 "empty value",
		"unknown=success":          "unknown key",
		"context.=ok":              "empty context name",
		"outcome==success":         "double equals",
		"outcome>=success":         "unsupported operator",
		"outcome=success &&":       "trailing conjunction",
		"&& outcome=success":       "leading conjunction",
		"outcome=success && bad=1": "one bad clause",
	}
	for cond, reason := range invalid {
		_, err := parseCondition(cond)
		if err == nil {
			t.Errorf("condition %q must be rejected (%s)", cond, reason)
			continue
		}
		if !strings.Contains(err.Error(), cond) && !strings.Contains(err.Error(), "clause") {
			t.Errorf("error %q must name the reason for %q", err.Error(), cond)
		}
	}
}
