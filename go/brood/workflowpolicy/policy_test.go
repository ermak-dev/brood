package workflowpolicy

import (
	"reflect"
	"strings"
	"testing"
)

func verified(paths ...string) Evidence {
	return Evidence{Known: true, Complete: true, Artifact: "artifact-1", ProducerUID: "producer", Paths: paths}
}
func evaluate(t *testing.T, rules []Rule, input Input) Decision {
	t.Helper()
	policy, err := New(rules)
	if err != nil {
		t.Fatal(err)
	}
	result, err := policy.Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProtectedPathsUseRecursiveRepositoryGlobs(t *testing.T) {
	rules := []Rule{{ID: "storage", Paths: []string{"go/brood/store/**"}, Require: Human}}
	for _, tc := range []struct {
		path string
		want string
	}{
		{"go/brood/store/data.go", WaitingHuman}, {"go/brood/store/tests/data.go", WaitingHuman},
		{"go/brood/store", WaitingHuman}, {"go/brood/storage/data.go", Allowed}, {"web/go/brood/store/data.go", Allowed},
	} {
		result := evaluate(t, rules, Input{Evidence: verified(tc.path)})
		if result.State != tc.want {
			t.Fatalf("path %q: state=%s want=%s", tc.path, result.State, tc.want)
		}
	}
	result := evaluate(t, []Rule{{ID: "deep", Paths: []string{"**/schema?.sql"}, Require: Human}}, Input{Evidence: verified("schema1.sql", "go/test/schema2.sql")})
	if result.State != WaitingHuman || !reflect.DeepEqual(result.Matches[0].Paths, []string{"go/test/schema2.sql", "schema1.sql"}) {
		t.Fatalf("recursive path matching: %+v", result)
	}
}

func TestUnknownEvidenceFailsClosedWithoutInventingMatches(t *testing.T) {
	rule := Rule{ID: "store", Paths: []string{"go/**"}, Require: Human}
	for _, evidence := range []Evidence{{}, {Known: true, Artifact: "artifact-1", ProducerUID: "producer"}, {Known: true, Complete: true, ProducerUID: "producer"}} {
		result := evaluate(t, []Rule{rule}, Input{Evidence: evidence})
		if result.State != Blocked || len(result.Unknown) != 1 || len(result.Matches) != 0 {
			t.Fatalf("unknown became no-match: %+v", result)
		}
	}
	result := evaluate(t, []Rule{rule}, Input{Evidence: verified()})
	if result.State != Allowed {
		t.Fatal("verified empty diff should not match protected paths")
	}
}

func TestConditionsConjoinAndRulesAccumulate(t *testing.T) {
	rules := []Rule{
		{ID: "paths-and-action", Paths: []string{"go/**"}, Actions: []string{"landing", "deploy"}, Require: Human},
		{ID: "review", Actions: []string{"landing"}, Require: IndependentReview, ReviewRole: "review"},
	}
	result := evaluate(t, rules, Input{ActionsKnown: true, Actions: []string{"work"}})
	if result.State != Allowed {
		t.Fatal("definitely unrelated action requires no path evidence")
	}
	result = evaluate(t, rules, Input{ActionsKnown: true, Actions: []string{"landing"}, Evidence: verified("go/store.go")})
	if result.State != WaitingHuman || !reflect.DeepEqual(result.Human, []string{"paths-and-action"}) || !reflect.DeepEqual(result.Review, []string{"review"}) {
		t.Fatalf("requirements did not accumulate: %+v", result)
	}
	result = evaluate(t, rules, Input{ActionsKnown: true, Actions: []string{"landing"}, Evidence: verified("docs/readme.md")})
	if result.State != WaitingReview || len(result.Human) != 0 {
		t.Fatalf("AND condition broadened: %+v", result)
	}
}

func TestUnknownActionClassificationDoesNotDefaultToWork(t *testing.T) {
	result := evaluate(t, []Rule{{ID: "land", Actions: []string{"landing"}, Require: Human}}, Input{Evidence: verified()})
	if result.State != Blocked || !reflect.DeepEqual(result.Unknown, []string{"land"}) {
		t.Fatalf("unclassified actor bypassed action policy: %+v", result)
	}
}

func TestDenyDominatesApprovalAndMissingEvidence(t *testing.T) {
	rules := []Rule{
		{ID: "human", Actions: []string{"landing"}, Require: Human},
		{ID: "deny", Actions: []string{"landing"}, Require: Deny},
		{ID: "paths", Paths: []string{"go/**"}, Require: Human},
	}
	result := evaluate(t, rules, Input{ActionsKnown: true, Actions: []string{"landing"}, Approved: map[string]bool{"human": true, "deny": true}})
	if result.State != Denied || !reflect.DeepEqual(result.Denied, []string{"deny"}) {
		t.Fatalf("deny overridden: %+v", result)
	}
}

func TestIndependentReviewCannotProceedWithoutArtifactEvidence(t *testing.T) {
	result := evaluate(t, []Rule{{ID: "review", Actions: []string{"landing"}, Require: IndependentReview, ReviewRole: "review"}}, Input{ActionsKnown: true, Actions: []string{"landing"}})
	if result.State != Blocked || !reflect.DeepEqual(result.Unknown, []string{"review"}) {
		t.Fatalf("review scheduled without an artifact: %+v", result)
	}
}

func TestIndependentReviewRequiresSameArtifactAndAnotherUID(t *testing.T) {
	rules := []Rule{{ID: "review", Actions: []string{"landing"}, Require: IndependentReview, ReviewRole: "review"}}
	input := Input{ActionsKnown: true, Actions: []string{"landing"}, Evidence: verified("go/a.go")}
	for _, review := range []ReviewEvidence{
		{Artifact: "artifact-1", ActorUID: "producer", Role: "review", Success: true},
		{Artifact: "old-artifact", ActorUID: "other", Role: "review", Success: true},
		{Artifact: "artifact-1", ActorUID: "other", Role: "build", Success: true},
		{Artifact: "artifact-1", ActorUID: "other", Role: "review", Success: false},
	} {
		input.Reviews = []ReviewEvidence{review}
		if result := evaluate(t, rules, input); result.State != WaitingReview {
			t.Fatalf("unproven review accepted: %+v", review)
		}
	}
	input.Reviews = []ReviewEvidence{{Artifact: "artifact-1", ActorUID: "other", Role: "review", Success: true}}
	if result := evaluate(t, rules, input); result.State != Allowed {
		t.Fatalf("valid review not consumed: %+v", result)
	}
}

func TestPolicyValidationAndDetachedInputs(t *testing.T) {
	for _, rule := range []Rule{
		{ID: "x", Require: Human}, {ID: "x", Actions: []string{"unknown"}, Require: Human},
		{ID: "x", Paths: []string{"../private-value"}, Require: Human},
		{ID: "x", Paths: []string{"/private-value"}, Require: Human},
		{ID: "x", Paths: []string{"go/**x"}, Require: Human},
		{ID: "x", Paths: []string{"go/["}, Require: Human},
		{ID: "x", Actions: []string{"landing"}, Require: "unknown"},
		{ID: "x", Actions: []string{"landing"}, Require: IndependentReview},
		{ID: "x", Actions: []string{"landing"}, Require: Human, ReviewRole: "review"},
	} {
		if p, err := New([]Rule{rule}); err == nil || p != nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("invalid rule accepted: %v", err)
		}
	}
	if _, err := New([]Rule{{ID: "x", Actions: []string{"landing"}, Require: Human}, {ID: "x", Actions: []string{"landing"}, Require: Deny}}); err == nil {
		t.Fatal("duplicate rule identity accepted")
	}
	rules := []Rule{{ID: "stable", Paths: []string{"go/**"}, Require: Human}}
	p, err := New(rules)
	if err != nil {
		t.Fatal(err)
	}
	rules[0].Paths[0] = "docs/**"
	result, err := p.Evaluate(Input{Evidence: verified("go/a.go")})
	if err != nil || result.State != WaitingHuman {
		t.Fatal("caller rewrote compiled policy")
	}
}

func TestInvalidEvidenceIsNotSilentlyTruncatedOrNormalized(t *testing.T) {
	p, err := New([]Rule{{ID: "paths", Paths: []string{"**"}, Require: Human}})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"/absolute", "a/../b", "a//b", "a\\b", string([]byte{0xff}), "a\x00b"} {
		if decision, err := p.Evaluate(Input{Evidence: verified(value)}); err == nil || decision.State == Allowed {
			t.Fatalf("malformed evidence produced permission: path=%q decision=%+v error=%v", value, decision, err)
		}
	}
	if _, err := p.Evaluate(Input{Evidence: verified(make([]string, 2049)...)}); err == nil {
		t.Fatal("oversized evidence accepted")
	}
}
