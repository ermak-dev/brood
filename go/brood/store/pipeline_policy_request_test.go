package store

// Policy-вопрос — проекция durable candidate/confirmation, а не human visit
// и не вопрос актора. Его ответ не попадает в actor delivery.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPolicyRequestAppearsWithoutHumanNodeAndRoutesToConfirmation(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	id := "policy:" + candidate.ID
	requests, err := s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	if len(requests) != 1 {
		t.Fatalf("policy question missing from shared inbox: %+v", requests)
	}
	request := requests[0]
	if request.ID != id || request.Origin != "policy" || request.Kind != "confirm" || request.State != "open" || request.Sequence == nil || *request.Sequence != 1 || request.ActorUID != "" || request.Delivery != "" {
		t.Fatalf("policy question became actor/human-node work: %+v", request)
	}
	if !strings.Contains(request.Context, candidate.ID) || !strings.Contains(request.Context, "storage") || strings.Contains(request.Context, "Only docs changed") {
		t.Fatalf("policy question lost trusted identity or copied report prose: %q", request.Context)
	}
	if stepCount(t, s) != 1 || stepOf(t, s, 1).NodeID != "build" {
		t.Fatal("inbox projection inserted a human graph visit")
	}
	in := SwarmAnswer{SwarmID: "one", RequestID: id, UserID: "u", Response: "yes", ExpectedRevision: 1}
	answered, err := s.AnswerSwarmRequest(ctx, in, allow)
	mustNil(t, err)
	if answered.State != "answered" || answered.AnsweredBy != "u" || answered.Response == nil || *answered.Response != "yes" || answered.Outcome != nil || answered.Delivery != "" {
		t.Fatalf("policy answer was routed as actor delivery/graph outcome: %+v", answered)
	}
	confirmation, err := s.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID)
	mustNil(t, err)
	if confirmation.Answer != "yes" || confirmation.Revision != 2 || stepCount(t, s) != 1 {
		t.Fatal("shared request answer did not use the one-shot ledger")
	}
	if _, err := s.AnswerSwarmRequest(ctx, in, allow); err != nil {
		t.Fatalf("exact shared-answer retry failed: %v", err)
	}
	in.Response = "no"
	if _, err := s.AnswerSwarmRequest(ctx, in, allow); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("policy answer overwritten through generic route: %v", err)
	}
}

func TestPolicyRequestSharedRouteKeepsAuthorityAndRevisionChecks(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	id := "policy:" + candidate.ID
	in := SwarmAnswer{SwarmID: "one", RequestID: id, UserID: "v", Response: "yes", ExpectedRevision: 1}
	if _, err := s.AnswerSwarmRequest(ctx, in, allow); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("shared policy route accepted viewer: %v", err)
	}
	in.UserID, in.ExpectedRevision = "u", 0
	if _, err := s.AnswerSwarmRequest(ctx, in, allow); !errors.Is(err, ErrPolicyConfirmationConflict) {
		t.Fatalf("shared policy route inferred an omitted revision: %v", err)
	}
	in.ExpectedRevision, in.Outcome = 1, "success"
	if _, err := s.AnswerSwarmRequest(ctx, in, allow); !errors.Is(err, ErrSwarmRequestInvalid) {
		t.Fatalf("shared policy route accepted a graph outcome: %v", err)
	}
	if _, err := s.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("refused shared answer wrote an approval")
	}
}

func TestPolicyRequestStopAndForeignLookupCannotReopenCandidate(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	id := "policy:" + candidate.ID
	if _, err := s.GetSwarmRequest(ctx, "two", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign policy request exposed material: %v", err)
	}
	stopOne(t, s)
	request, err := s.GetSwarmRequest(ctx, "one", id)
	mustNil(t, err)
	if request.State != "cancelled" || request.Response != nil {
		t.Fatalf("stop left unanswered policy question actionable: %+v", request)
	}
	if _, err := s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: id, UserID: "u", Response: "yes", ExpectedRevision: 1}, allow); err == nil {
		t.Fatal("late policy answer resumed a stopped pass")
	}
}
