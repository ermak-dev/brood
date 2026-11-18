package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// BUG: Прямой AnswerPipelinePolicy обходит запрет живого human approval в eval.
// HYPOTHESIS: Eval fence есть лишь в AnswerSwarmRequest, но entry engine читает
// подтверждения нижнего API; отказ нужен до свежего ответа и точного retry.
func TestSwarmEvalRefusesDirectPolicyConfirmation(t *testing.T) {
	for _, answer := range []string{"yes", "no"} {
		t.Run(answer, func(t *testing.T) {
			ctx := context.Background()
			s, in := evalActorFixture(t, true)
			request, err := s.GetSwarmRequest(ctx, in.CandidateSwarmID, in.RequestID)
			mustNil(t, err)
			candidateID := strings.TrimPrefix(request.ID, policyRequestPrefix)
			if candidateID == request.ID || candidateID == "" {
				t.Fatalf("fixture has no actual policy question: %+v", request)
			}
			before := databaseRows(t, s.db)
			for range 2 {
				_, err = s.AnswerPipelinePolicy(ctx, PipelinePolicyAnswer{SwarmID: in.CandidateSwarmID, CandidateID: candidateID, UserID: in.LauncherID, ExpectedRevision: 1, Answer: answer}, allow)
				if !errors.Is(err, ErrSwarmEvalConflict) {
					t.Fatalf("direct %s policy answer bypassed eval fence: %v", answer, err)
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refused eval policy answer changed retained history")
			}
			if _, err := s.GetPipelinePolicyConfirmation(ctx, in.CandidateSwarmID, candidateID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("eval acquired live human policy confirmation: %v", err)
			}
		})
	}
}
