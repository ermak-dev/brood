package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
)

func policyTelegramResult(t *testing.T) (*Store, PipelineResultObservation) {
	t.Helper()
	s, observation, signed := policyPreviewResult(t)
	mustNil(t, s.RecordPipelineResultEvidence(context.Background(), observation, attest.ResultKey(resultTestToken), signed))
	seedUser(t, s, "outsider-policy", "outsider-policy")
	for i, user := range []string{"root", "u", "v", "outsider-policy"} {
		linkTelegram(t, s, user, "", int64(910+i))
	}
	return s, observation
}

func TestPolicyTelegramQueuesOpenAndAnswerOnceForCurrentLinkedReaders(t *testing.T) {
	ctx := context.Background()
	s, observation := policyTelegramResult(t)
	candidate, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "result", "notify-candidate")
	mustNil(t, err)
	ref := "swarm/one/request/policy:" + candidate.ID
	if got := queuedOf(t, s, "request_opened"); !reflect.DeepEqual(got, readers(ref)) {
		t.Fatalf("policy open notification recipients: got %v want %v", got, readers(ref))
	}
	for _, user := range []string{"root", "u", "v"} {
		events := eventsOf(t, s, user, "request_opened")
		want := []TelegramEvent{{Swarm: "one", Request: "policy:" + candidate.ID, Sequence: 1, Node: "build"}}
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("policy event lost exact source scope: %#v", events)
		}
	}
	retry, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "result", "different-retry-id")
	mustNil(t, err)
	if retry.ID != candidate.ID || len(queuedOf(t, s, "request_opened")) != 3 {
		t.Fatal("candidate retry duplicated notification")
	}
	for range 2 {
		_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
		mustNil(t, err)
	}
	if got := queuedOf(t, s, "request_answered"); !reflect.DeepEqual(got, readers(ref)) {
		t.Fatalf("policy answer duplicated or lost reader event: %v", got)
	}
	if len(queued(t, s)) != 6 {
		t.Fatal("policy notification invented actor delivery or graph/pass event")
	}
	if stepCount(t, s) != 1 || stepOf(t, s, 1).Outcome != nil {
		t.Fatal("notification applied transition")
	}
	intent, err := s.GetPipelineCompletionIntent(ctx, "one", 1)
	mustNil(t, err)
	if intent.Report != observation.Intent.Report {
		t.Fatal("notification rewrote actor report")
	}
}

func TestPolicyTelegramOutboxFailureRollsBackCandidateAndAnswer(t *testing.T) {
	ctx := context.Background()
	s, _ := policyTelegramResult(t)
	_, err := s.db.Exec(`CREATE TRIGGER reject_policy_notification BEFORE INSERT ON telegram_outbox WHEN NEW.kind='request_opened' BEGIN SELECT RAISE(ABORT,'notification unavailable'); END`)
	mustNil(t, err)
	if _, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "result", "atomic-candidate"); err == nil {
		t.Fatal("candidate committed without its notification")
	}
	if _, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed outbox retained candidate: %v", err)
	}
	if len(queued(t, s)) != 0 {
		t.Fatal("rolled back candidate left notification")
	}
	_, err = s.db.Exec(`DROP TRIGGER reject_policy_notification`)
	mustNil(t, err)
	candidate, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "result", "atomic-candidate")
	mustNil(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER reject_policy_answer_notification BEFORE INSERT ON telegram_outbox WHEN NEW.kind='request_answered' BEGIN SELECT RAISE(ABORT,'notification unavailable'); END`)
	mustNil(t, err)
	if _, err := s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "no"), allow); err == nil {
		t.Fatal("answer committed without its notification")
	}
	if _, err := s.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outbox failure retained answer: %v", err)
	}
	if len(queuedOf(t, s, "request_answered")) != 0 {
		t.Fatal("failed answer left a notification")
	}
}

func TestPolicyTelegramOuterRollbackLeavesNeitherCandidateNorEvent(t *testing.T) {
	ctx := context.Background()
	s, _ := policyTelegramResult(t)
	rollback := errors.New("rollback")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "result", "rolled-back")
		if err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if _, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result"); !errors.Is(err, ErrNotFound) {
		t.Fatal("outer rollback retained candidate")
	}
	if len(queued(t, s)) != 0 {
		t.Fatal("outer rollback retained policy notification")
	}
}
