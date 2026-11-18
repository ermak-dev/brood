package store

import (
	"context"
	"strings"
	"testing"
)

// FEATURE: Durable answer использует source вопроса точного ephemeral child (#292).
// SPEC: Те же FULL intent/journal/attempts, без workflow role; завершённый child
// не получает новый send, а исходные bytes остаются историей.
// IMPLEMENTATION: Answer source проверяет child context вместо workflow-role eligibility.
// RESULT: IMPLEMENTED — focused/race и полный Go build/test/vet прошли.
// STATUS: DONE
func TestEphemeralAnswerDurableSourceRetainsFrameAndRefusesCompletion(t *testing.T) {
	ctx := context.Background()
	s, intent, child, in := ephemeralQuestionFixture(t)
	q, err := s.OpenEphemeralChildRequest(ctx, child.ID, in, childAllow)
	mustNil(t, err)
	mustNil(t, answer(s, q, "u", "", "Only the isolated fixture."))
	input := ActorDeliveryInput{SwarmID: q.SwarmID, SourceKind: "answer", SourceID: q.ID, ActorUID: q.ActorUID, Incarnation: q.ActorIncarnation, AtespaceUID: child.Executor.AtespaceUID, JournalID: strings.Repeat("b", 64), Payload: `{"to":"isolated","from":"operator:u","body":"Original scoped answer.","waitSec":0}`}
	delivery, err := s.PrepareActorDelivery(ctx, input, allow)
	if err != nil {
		t.Fatalf("proof-bound running child answer cannot reserve durable transport: %v", err)
	}
	if delivery.Input != input || delivery.ProbeOnly {
		t.Fatal("scoped answer changed bytes or invented prior delivery")
	}
	mustNil(t, s.CheckActorDeliveryCurrent(ctx, delivery.ID))
	_, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision, "success", "Independent result.", childAllow)
	mustNil(t, err)
	if err := s.CheckActorDeliveryCurrent(ctx, delivery.ID); err == nil {
		t.Fatal("completed child still authorizes answer transport")
	}
	retained, err := s.GetActorDelivery(ctx, delivery.ID)
	mustNil(t, err)
	if retained != delivery {
		t.Fatal("scope refusal discarded original answer history")
	}
}
