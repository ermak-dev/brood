// Package workflowpolicy evaluates automatic transition requirements without effects.
// Its inputs are trusted adapter observations, not actor-supplied evidence claims.
package workflowpolicy

import (
	"errors"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

// Requirements constrain automatic orchestration, not manual owner authority.
const (
	Human             = "human"
	IndependentReview = "independent_review"
	Deny              = "deny"
	Allowed           = "allowed"
	WaitingHuman      = "waiting_human"
	WaitingReview     = "waiting_review"
	Denied            = "denied"
	Blocked           = "blocked"
)

// Rule combines supplied conditions with AND; alternatives inside each list use OR.
type Rule struct {
	ID         string   `json:"id"`
	Paths      []string `json:"paths,omitempty"`
	Actions    []string `json:"actions,omitempty"`
	Require    string   `json:"require"`
	ReviewRole string   `json:"reviewRole,omitempty"`
}

// Evidence is complete trusted change evidence for an immutable artifact.
// Unknown or incomplete evidence cannot establish that a protected path is absent.
type Evidence struct {
	Known, Complete       bool
	Artifact, ProducerUID string
	Paths                 []string
}

// ReviewEvidence is a trusted successful review assignment/result, not report prose.
// The store adapter must bind it to the exact current pass and artifact.
type ReviewEvidence struct {
	Artifact, ActorUID, Role string
	Success                  bool
}

// Input contains the candidate's declared classes and durable evidence.
// Approved entries must come from confirmations bound to this exact transition;
// this pure evaluator does not authenticate sessions or confirmation records.
type Input struct {
	// TargetKind is actor (also the legacy empty value), human, or exit.
	// Non-actor candidates have no actor action classification.
	TargetKind string
	// Initial is trusted pass history: no actor result has been produced yet.
	// Missing or refused evidence alone must never establish this context.
	Initial      bool
	ActionsKnown bool
	Actions      []string
	Evidence     Evidence
	Approved     map[string]bool
	Reviews      []ReviewEvidence
}

// Match contains only known matching paths; unknown conditions are kept separately.
type Match struct {
	RuleID string
	Paths  []string
}

// Decision accumulates requirements. Only Allowed permits automatic progression.
type Decision struct {
	State                          string
	Matches                        []Match
	Human, Review, Denied, Unknown []string
	// NotApplicable lists path rules deferred at a safe initial boundary.
	NotApplicable []string
}

type compiledRule struct {
	rule     Rule
	patterns [][]string
}

// Policy owns detached validated rules; evaluation does not mutate it.
type Policy struct{ rules []compiledRule }

var errRule = errors.New("invalid workflow policy rule")
var errEvidence = errors.New("invalid workflow policy evidence")

// ValidAction reports supported explicit node classifications.
func ValidAction(action string) bool {
	switch action {
	case "work", "review", "landing", "schema", "deploy", "destructive_lifecycle":
		return true
	}
	return false
}
func identifier(value string) bool {
	if len(value) < 1 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func relative(value string, pattern bool) ([]string, error) {
	if !utf8.ValidString(value) || len(value) == 0 || len(value) > 1024 || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return nil, errRule
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return nil, errRule
		}
	}
	parts := strings.Split(value, "/")
	if len(parts) > 128 {
		return nil, errRule
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errRule
		}
		if pattern {
			if strings.Contains(part, "**") && part != "**" {
				return nil, errRule
			}
			if _, err := path.Match(part, ""); err != nil {
				return nil, errRule
			}
		}
	}
	return parts, nil
}

// New validates bounded rules, preserving an empty policy as ordinary routing.
func New(rules []Rule) (*Policy, error) {
	if len(rules) > 64 {
		return nil, errRule
	}
	p := &Policy{}
	seen := map[string]bool{}
	for _, input := range rules {
		if !identifier(input.ID) || seen[input.ID] || len(input.Paths)+len(input.Actions) == 0 || len(input.Paths) > 32 || len(input.Actions) > 6 {
			return nil, errRule
		}
		if input.Require != Human && input.Require != IndependentReview && input.Require != Deny {
			return nil, errRule
		}
		if input.Require == IndependentReview {
			if !identifier(input.ReviewRole) {
				return nil, errRule
			}
		} else if input.ReviewRole != "" {
			return nil, errRule
		}
		seen[input.ID] = true
		input.Paths = slices.Clone(input.Paths)
		input.Actions = slices.Clone(input.Actions)
		compiled := compiledRule{rule: input}
		for _, pattern := range input.Paths {
			parts, err := relative(pattern, true)
			if err != nil {
				return nil, errRule
			}
			compiled.patterns = append(compiled.patterns, parts)
		}
		for _, action := range input.Actions {
			if !ValidAction(action) {
				return nil, errRule
			}
		}
		p.rules = append(p.rules, compiled)
	}
	slices.SortFunc(p.rules, func(a, b compiledRule) int { return strings.Compare(a.rule.ID, b.rule.ID) })
	return p, nil
}

func matches(pattern, parts []string) bool {
	type position struct{ p, s int }
	memo := map[position]bool{}
	seen := map[position]bool{}
	var visit func(int, int) bool
	visit = func(p, s int) bool {
		key := position{p, s}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		result := false
		switch {
		case p == len(pattern):
			result = s == len(parts)
		case pattern[p] == "**":
			result = visit(p+1, s) || s < len(parts) && visit(p, s+1)
		case s < len(parts):
			ok, _ := path.Match(pattern[p], parts[s])
			result = ok && visit(p+1, s+1)
		}
		memo[key] = result
		return result
	}
	return visit(0, 0)
}

func hasReview(input Input, rule Rule) bool {
	e := input.Evidence
	if !e.Known || !e.Complete || e.Artifact == "" || e.ProducerUID == "" {
		return false
	}
	for _, review := range input.Reviews {
		if review.Success && review.Artifact == e.Artifact && review.ActorUID != "" && review.ActorUID != e.ProducerUID && review.Role == rule.ReviewRole {
			return true
		}
	}
	return false
}

// Evaluate rejects invalid/over-budget inputs, never truncating evidence into a no-match.
// The caller must persist the decision and withhold automatic dispatch until Allowed.
func (p *Policy) Evaluate(input Input) (Decision, error) {
	result := Decision{State: Allowed}
	if p == nil || len(input.Actions) > 6 || len(input.Evidence.Paths) > 2048 || len(input.Reviews) > 64 || len(input.Approved) > 64 || len(input.Evidence.Artifact) > 256 || len(input.Evidence.ProducerUID) > 128 {
		return Decision{}, errEvidence
	}
	noActor := input.TargetKind == "human" || input.TargetKind == "exit"
	if input.TargetKind != "" && input.TargetKind != "actor" && !noActor {
		return Decision{}, errEvidence
	}
	if noActor && len(input.Actions) != 0 || !noActor && input.ActionsKnown && len(input.Actions) == 0 {
		return Decision{}, errEvidence
	}
	if input.Initial && (input.Evidence.Known || input.Evidence.Complete || input.Evidence.Artifact != "" || input.Evidence.ProducerUID != "" || input.Evidence.Paths != nil || len(input.Reviews) != 0) {
		return Decision{}, errEvidence
	}
	for _, action := range input.Actions {
		if !ValidAction(action) {
			return Decision{}, errEvidence
		}
	}
	initialPaths := input.Initial && (noActor || input.ActionsKnown && len(input.Actions) == 1 && input.Actions[0] == "work")
	paths := slices.Clone(input.Evidence.Paths)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	parts := make([][]string, len(paths))
	for i, value := range paths {
		var err error
		parts[i], err = relative(value, false)
		if err != nil {
			return Decision{}, errEvidence
		}
	}
	complete := input.Evidence.Known && input.Evidence.Complete && input.Evidence.Artifact != "" && input.Evidence.ProducerUID != ""
	work := 0
	for _, compiled := range p.rules {
		rule := compiled.rule
		actionMatch, actionUnknown := len(rule.Actions) == 0, false
		if len(rule.Actions) > 0 {
			if noActor {
				continue
			}
			if !input.ActionsKnown {
				actionUnknown = true
			} else {
				for _, action := range rule.Actions {
					actionMatch = actionMatch || slices.Contains(input.Actions, action)
				}
			}
		}
		if !actionMatch && !actionUnknown {
			continue
		}
		pathMatch, pathUnknown := len(rule.Paths) == 0, false
		var matchedPaths []string
		if len(rule.Paths) > 0 {
			if initialPaths {
				result.NotApplicable = append(result.NotApplicable, rule.ID)
				continue
			}
			if !complete {
				pathUnknown = true
			} else {
				for i, parts := range parts {
					for j, pattern := range compiled.patterns {
						work += (len(rule.Paths[j]) + 1) * (len(paths[i]) + 1)
						if work > 32<<20 {
							return Decision{}, errors.New("workflow policy evaluation limit exceeded")
						}
						if matches(pattern, parts) {
							pathMatch = true
							matchedPaths = append(matchedPaths, paths[i])
							break
						}
					}
				}
			}
		}
		if !pathMatch && !pathUnknown {
			continue
		}
		if pathUnknown || actionUnknown {
			result.Unknown = append(result.Unknown, rule.ID)
			continue
		}
		result.Matches = append(result.Matches, Match{RuleID: rule.ID, Paths: matchedPaths})
		switch rule.Require {
		case Deny:
			result.Denied = append(result.Denied, rule.ID)
		case Human:
			if !input.Approved[rule.ID] {
				result.Human = append(result.Human, rule.ID)
			}
		case IndependentReview:
			if !complete {
				result.Unknown = append(result.Unknown, rule.ID)
			} else if !hasReview(input, rule) {
				result.Review = append(result.Review, rule.ID)
			}
		}
	}
	switch {
	case len(result.Denied) > 0:
		result.State = Denied
	case len(result.Unknown) > 0:
		result.State = Blocked
	case len(result.Human) > 0:
		result.State = WaitingHuman
	case len(result.Review) > 0:
		result.State = WaitingReview
	}
	return result, nil
}
