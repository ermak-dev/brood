package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func replayQuestion(swarm, id string, sequence int64) SwarmRequest {
	return SwarmRequest{ID: id, SwarmID: swarm, Sequence: &sequence, Origin: "node", Kind: "choice", State: "open", Question: "Continue this recorded branch?", Options: []RequestOption{{Label: "Yes", Outcome: "yes"}, {Label: "No", Outcome: "no"}}}
}

func replayReference() SwarmPassTrace {
	trace := comparisonTrace("reference", 1, "approve", "approve", "other")
	first, second, third := replayQuestion("reference", "first", 1), replayQuestion("reference", "second", 2), replayQuestion("reference", "third", 3)
	yes, no := "yes", "no"
	first.State, second.State, third.State = "answered", "answered", "answered"
	first.Outcome, second.Outcome, third.Outcome = &yes, &no, &yes
	first.AnsweredBy, second.AnsweredBy, third.AnsweredBy = "original-human", "original-human", "original-human"
	response := "Original additional text.\nNot a new permission."
	first.Response = &response
	trace.Requests = []SwarmPassTraceRequest{{Request: third}, {Request: second}, {Request: first}}
	return trace
}

// Сопоставление replay (#293) выбирает данные по узлу и номеру посещения.
// Старый ответ не является новой политикой или живым подтверждением.
func TestMatchSwarmReplayAnswerUsesNodeOccurrenceAndExactVariants(t *testing.T) {
	reference := replayReference()
	question := replayQuestion("eval-candidate", "candidate-question", 20)
	question.Options = []RequestOption{question.Options[1], question.Options[0]}
	before, _ := json.Marshal(reference)
	first, err := MatchSwarmReplayAnswer(reference, "approve", 1, question)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReferenceSwarmID != "reference" || first.ReferencePass != 1 || first.ReferenceSequence != 1 || first.ReferenceRequestID != "first" || first.NodeID != "approve" || first.Occurrence != 1 || first.CandidateSwarmID != "eval-candidate" || first.CandidateSequence != 20 || first.CandidateRequestID != "candidate-question" || first.Outcome != "yes" || first.RecordedBy != "original-human" || first.Response == nil || *first.Response != "Original additional text.\nNot a new permission." {
		t.Fatalf("matcher lost exact source/target answer scope: %+v", first)
	}
	repeated, err := MatchSwarmReplayAnswer(reference, "approve", 1, question)
	if err != nil || !reflect.DeepEqual(first, repeated) {
		t.Fatal("reading the same replay answer consumed it or changed it")
	}
	second, err := MatchSwarmReplayAnswer(reference, "approve", 2, question)
	if err != nil || second.ReferenceSequence != 2 || second.Outcome != "no" {
		t.Fatal("repeated node reused the first occurrence")
	}
	other, err := MatchSwarmReplayAnswer(reference, "other", 1, question)
	if err != nil || other.ReferenceSequence != 3 || other.Outcome != "yes" {
		t.Fatal("different node was matched by global question order")
	}
	*first.Response = "changed output"
	after, _ := json.Marshal(reference)
	if string(before) != string(after) {
		t.Fatal("matcher mutated reference history or shared response pointers")
	}
}

func TestMatchSwarmReplayAnswerReadsRealHistoryWithoutApplyingIt(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	_, err := startWithGoal(s, "release", "Reference goal")
	mustNil(t, err)
	original := openNodeRequestOf(t, s, 1)
	_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: original.ID, UserID: "u", Outcome: "success", Response: "Retained operator notes."}, allow)
	mustNil(t, err)
	stopOne(t, s)
	_, err = startWithGoal(s, "release", "A separate current goal")
	mustNil(t, err)
	candidate := openNodeRequestOf(t, s, 3)
	reference, err := s.GetSwarmPassTrace(ctx, "one", 1)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	selected, err := MatchSwarmReplayAnswer(reference, "ask", 1, candidate)
	mustNil(t, err)
	if selected.ReferenceRequestID != original.ID || selected.CandidateRequestID != candidate.ID || selected.CandidateSequence != 3 || selected.Outcome != "success" || selected.RecordedBy != "u" || selected.Response == nil || *selected.Response != "Retained operator notes." {
		t.Fatalf("real request history lost source/target binding: %+v", selected)
	}
	current, err := s.GetSwarmRequest(ctx, "one", candidate.ID)
	mustNil(t, err)
	if current.State != "open" || current.Outcome != nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("matching consumed/applied an answer or changed execution/notifications/authority")
	}
}

func TestMatchSwarmReplayAnswerRejectsChangedUnknownAndNonGraphQuestions(t *testing.T) {
	reference := replayReference()
	for _, change := range []func(*SwarmRequest){
		func(q *SwarmRequest) { q.Origin = "actor"; q.Sequence = nil },
		func(q *SwarmRequest) { q.Origin = "policy" },
		func(q *SwarmRequest) { q.Kind = "confirm" },
		func(q *SwarmRequest) { q.State = "answered" },
		func(q *SwarmRequest) { q.Sequence = nil },
		func(q *SwarmRequest) { q.Options[0].Label = "Approve a different action" },
		func(q *SwarmRequest) { q.Options[0].Outcome = "proceed" },
		func(q *SwarmRequest) { q.Options = append(q.Options, RequestOption{Label: "Maybe", Outcome: "maybe"}) },
		func(q *SwarmRequest) { q.Options = append(q.Options, q.Options[0]) },
		func(q *SwarmRequest) { q.ActorUID = "an-actor" },
	} {
		question := replayQuestion("candidate", "question", 10)
		change(&question)
		answer, err := MatchSwarmReplayAnswer(reference, "approve", 1, question)
		if !errors.Is(err, ErrSwarmReplayMismatch) || !reflect.DeepEqual(answer, SwarmReplayAnswer{}) {
			t.Fatal("unmatched question produced a partial or authoritative answer")
		}
	}
	for _, selection := range []struct {
		node       string
		occurrence int
	}{{"missing", 1}, {"approve", 3}, {"approve", 0}, {"approve", -1}} {
		answer, err := MatchSwarmReplayAnswer(reference, selection.node, selection.occurrence, replayQuestion("candidate", "question", 10))
		if !errors.Is(err, ErrSwarmReplayMismatch) || !reflect.DeepEqual(answer, SwarmReplayAnswer{}) {
			t.Fatal("nonexistent occurrence produced an answer")
		}
	}
}

func TestMatchSwarmReplayAnswerRefusesUnrecordedAmbiguousOrForeignHistory(t *testing.T) {
	for _, change := range []func(*SwarmPassTrace){
		func(v *SwarmPassTrace) { v.Version = 2 },
		func(v *SwarmPassTrace) { v.Requests = nil },
		func(v *SwarmPassTrace) { v.Requests[2].Request.State = "open"; v.Requests[2].Request.Outcome = nil },
		func(v *SwarmPassTrace) { v.Requests[2].Request.Outcome = nil },
		func(v *SwarmPassTrace) { value := "missing-variant"; v.Requests[2].Request.Outcome = &value },
		func(v *SwarmPassTrace) { v.Requests[2].Request.SwarmID = "foreign" },
		func(v *SwarmPassTrace) { v.Requests[2].Request.Sequence = nil },
		func(v *SwarmPassTrace) { v.Requests = append(v.Requests, v.Requests[2]) },
		func(v *SwarmPassTrace) { v.Requests[2].Request.Options = nil },
		func(v *SwarmPassTrace) {
			value := strings.Repeat("x", MaxSwarmRequestTextBytes+1)
			v.Requests[2].Request.Response = &value
		},
	} {
		reference := replayReference()
		change(&reference)
		answer, err := MatchSwarmReplayAnswer(reference, "approve", 1, replayQuestion("candidate", "question", 10))
		if !errors.Is(err, ErrSwarmReplayMismatch) || !reflect.DeepEqual(answer, SwarmReplayAnswer{}) {
			t.Fatal("missing/foreign/ambiguous reference answer was invented")
		}
	}
}

func TestMatchSwarmReplayAnswerDoesNotUseActorOrPolicyApprovalsAsGraphAnswers(t *testing.T) {
	reference := replayReference()
	reference.Requests[2].Request.Origin = "actor"
	reference.Requests[2].Request.Sequence = nil
	actorQuestion := reference.Requests[2]
	reference.UnscopedActorRequests = []SwarmRequest{actorQuestion.Request}
	policy := replayQuestion("reference", "policy:old-confirmation", 1)
	yes := "yes"
	policy.Origin, policy.State, policy.Outcome = "policy", "answered", &yes
	reference.Requests = append(reference.Requests, SwarmPassTraceRequest{Request: policy})
	answer, err := MatchSwarmReplayAnswer(reference, "approve", 1, replayQuestion("candidate", "question", 10))
	if !errors.Is(err, ErrSwarmReplayMismatch) || !reflect.DeepEqual(answer, SwarmReplayAnswer{}) {
		t.Fatal("old actor/policy yes became a graph replay answer")
	}
	if strings.Contains(err.Error(), "old-confirmation") {
		t.Fatal("matcher reflected private question identifiers in its refusal")
	}
}
