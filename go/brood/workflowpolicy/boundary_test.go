package workflowpolicy

// Начальная граница задаётся доверенным store adapter, а не отсутствием evidence:
// проваленное наблюдение результата не превращает переход в начало прохода.

import (
	"reflect"
	"testing"
)

func TestInitialOrdinaryWorkDefersPathRulesWithoutInventingEvidence(t *testing.T) {
	rules := []Rule{
		{ID: "storage", Paths: []string{"go/brood/store/**"}, Require: Human},
		{ID: "land", Actions: []string{"landing"}, Require: Human},
	}
	input := Input{Initial: true, TargetKind: "actor", ActionsKnown: true, Actions: []string{"work"}}
	result := evaluate(t, rules, input)
	want := Decision{State: Allowed, NotApplicable: []string{"storage"}}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("initial work must defer path rules explicitly, not invent an empty artifact: got=%+v want=%+v", result, want)
	}
	if !reflect.DeepEqual(input.Evidence, Evidence{}) {
		t.Fatal("evaluation invented evidence for initial work")
	}

	// После обычного work результат проверяется даже на последнем ребре к exit.
	result = evaluate(t, rules, Input{TargetKind: "exit", Evidence: verified("go/brood/store/data.go")})
	if result.State != WaitingHuman || !reflect.DeepEqual(result.Human, []string{"storage"}) || len(result.NotApplicable) != 0 {
		t.Fatalf("deferred rule disappeared when the result became available: %+v", result)
	}
}

func TestInitialContextDoesNotDefaultUnknownOrProtectedActorToWork(t *testing.T) {
	rules := []Rule{{ID: "paths", Paths: []string{"go/**"}, Require: Human}}
	inputs := []Input{
		{Initial: true},
		{Initial: true, TargetKind: "actor", Actions: []string{"work"}},
	}
	for _, action := range []string{"review", "landing", "schema", "deploy", "destructive_lifecycle"} {
		inputs = append(inputs, Input{Initial: true, TargetKind: "actor", ActionsKnown: true, Actions: []string{action}})
	}
	for _, input := range inputs {
		result := evaluate(t, rules, input)
		if result.State != Blocked || !reflect.DeepEqual(result.Unknown, []string{"paths"}) || len(result.NotApplicable) != 0 || len(result.Matches) != 0 {
			t.Fatalf("initial context bypassed a missing artifact for %+v: %+v", input, result)
		}
	}
}

func TestInitialContextStillRequiresActionOnlyObligations(t *testing.T) {
	rules := []Rule{
		{ID: "human", Actions: []string{"work"}, Require: Human},
		{ID: "paths", Paths: []string{"go/**"}, Require: Human},
	}
	input := Input{Initial: true, TargetKind: "actor", ActionsKnown: true, Actions: []string{"work"}}
	result := evaluate(t, rules, input)
	if result.State != WaitingHuman || !reflect.DeepEqual(result.Human, []string{"human"}) || !reflect.DeepEqual(result.NotApplicable, []string{"paths"}) {
		t.Fatalf("initial context bypassed action approval: %+v", result)
	}
	input.Approved = map[string]bool{"human": true}
	if result = evaluate(t, rules, input); result.State != Allowed {
		t.Fatalf("exact initial action approval was not consumed: %+v", result)
	}
	rules = append(rules, Rule{ID: "deny", Actions: []string{"work"}, Require: Deny})
	if result = evaluate(t, rules, input); result.State != Denied || !reflect.DeepEqual(result.Denied, []string{"deny"}) {
		t.Fatalf("initial approval overrode action denial: %+v", result)
	}
}

func TestInitialActionReviewCannotInventAnArtifact(t *testing.T) {
	result := evaluate(t, []Rule{{ID: "review", Actions: []string{"work"}, Require: IndependentReview, ReviewRole: "review"}}, Input{Initial: true, TargetKind: "actor", ActionsKnown: true, Actions: []string{"work"}})
	if result.State != Blocked || !reflect.DeepEqual(result.Unknown, []string{"review"}) {
		t.Fatalf("initial work fabricated an independently reviewable artifact: %+v", result)
	}
}

func TestInitialContextRejectsEvidenceOrReviewsFromAnotherBoundary(t *testing.T) {
	policy, err := New([]Rule{{ID: "paths", Paths: []string{"go/**"}, Require: Human}})
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []Evidence{
		verified("go/protected.go"),
		{Known: true}, {Complete: true}, {Artifact: "old-artifact"},
		{ProducerUID: "old-producer"}, {Paths: []string{}}, {Paths: []string{"go/protected.go"}},
	} {
		input := Input{Initial: true, TargetKind: "actor", ActionsKnown: true, Actions: []string{"work"}, Evidence: evidence}
		if decision, err := policy.Evaluate(input); err == nil || !reflect.DeepEqual(decision, Decision{}) {
			t.Fatalf("initial context erased evidence: evidence=%+v decision=%+v error=%v", evidence, decision, err)
		}
	}
	input := Input{Initial: true, TargetKind: "actor", ActionsKnown: true, Actions: []string{"work"}, Reviews: []ReviewEvidence{{Artifact: "old-artifact", ActorUID: "other", Role: "review", Success: true}}}
	if decision, err := policy.Evaluate(input); err == nil || !reflect.DeepEqual(decision, Decision{}) {
		t.Fatalf("initial context accepted a review without a current artifact: decision=%+v error=%v", decision, err)
	}
}

func TestNonActorCandidateHasNoActorActionButRetainsPathRequirements(t *testing.T) {
	rules := []Rule{
		{ID: "paths", Paths: []string{"go/**"}, Require: Human},
		{ID: "land", Actions: []string{"landing"}, Require: Deny},
		{ID: "path-land", Paths: []string{"go/**"}, Actions: []string{"landing"}, Require: Deny},
	}
	for _, target := range []string{"human", "exit"} {
		for _, known := range []bool{false, true} {
			input := Input{TargetKind: target, ActionsKnown: known, Evidence: verified("go/a.go")}
			result := evaluate(t, rules, input)
			want := Decision{State: WaitingHuman, Matches: []Match{{RuleID: "paths", Paths: []string{"go/a.go"}}}, Human: []string{"paths"}}
			if !reflect.DeepEqual(result, want) {
				t.Fatalf("%s acquired an unknown/landing action or skipped path approval: got=%+v want=%+v", target, result, want)
			}
			input.Approved = map[string]bool{"paths": true}
			if result = evaluate(t, rules, input); result.State != Allowed {
				t.Fatalf("%s path approval not consumed: %+v", target, result)
			}
		}
	}
}

func TestNonActorCandidateStillRequiresIndependentReviewOfProducedArtifact(t *testing.T) {
	rules := []Rule{{ID: "paths", Paths: []string{"go/**"}, Require: IndependentReview, ReviewRole: "review"}}
	for _, target := range []string{"human", "exit"} {
		input := Input{TargetKind: target, Evidence: verified("go/a.go")}
		if result := evaluate(t, rules, input); result.State != WaitingReview {
			t.Fatalf("%s bypassed artifact review: %+v", target, result)
		}
		input.Reviews = []ReviewEvidence{{Artifact: "artifact-1", ActorUID: "other", Role: "review", Success: true}}
		if result := evaluate(t, rules, input); result.State != Allowed {
			t.Fatalf("%s ignored exact independent review: %+v", target, result)
		}
	}
}

func TestNonActorMissingEvidenceIsNotAnInitialBoundary(t *testing.T) {
	rules := []Rule{{ID: "paths", Paths: []string{"go/**"}, Require: Human}}
	for _, target := range []string{"human", "exit"} {
		if result := evaluate(t, rules, Input{TargetKind: target}); result.State != Blocked || !reflect.DeepEqual(result.Unknown, []string{"paths"}) {
			t.Fatalf("%s silently inferred initial context from missing evidence: %+v", target, result)
		}
		result := evaluate(t, rules, Input{TargetKind: target, Initial: true})
		if result.State != Allowed || !reflect.DeepEqual(result.NotApplicable, []string{"paths"}) || len(result.Matches) != 0 {
			t.Fatalf("%s fabricated changes before any actor result: %+v", target, result)
		}
	}
}

func TestInvalidCandidateContextCannotProducePermission(t *testing.T) {
	policy, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []Input{
		{TargetKind: "land"}, {TargetKind: "parallel"}, {TargetKind: "unknown"},
		{TargetKind: "actor", ActionsKnown: true},
		{TargetKind: "human", Actions: []string{"landing"}},
		{TargetKind: "exit", Actions: []string{"work"}},
		{TargetKind: "human", Actions: []string{"unknown"}},
	} {
		if decision, err := policy.Evaluate(input); err == nil || !reflect.DeepEqual(decision, Decision{}) {
			t.Fatalf("invalid context produced permission: input=%+v decision=%+v error=%v", input, decision, err)
		}
	}
}

func TestEmptyPolicyPreservesValidInitialAndNonActorBoundaries(t *testing.T) {
	for _, input := range []Input{
		{}, {Initial: true}, {Initial: true, TargetKind: "actor", ActionsKnown: true, Actions: []string{"work"}},
		{TargetKind: "human"}, {TargetKind: "exit", ActionsKnown: true},
	} {
		if result := evaluate(t, nil, input); !reflect.DeepEqual(result, Decision{State: Allowed}) {
			t.Fatalf("empty policy changed valid ordinary routing: input=%+v decision=%+v", input, result)
		}
	}
}
