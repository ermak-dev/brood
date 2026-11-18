package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// BUG: Трасса прохода теряет typed-инструкцию ephemeral child (#293).
// HYPOTHESIS: Сборщик читает только source_kind step и answer.
// SOLUTION: ChildInstructions читает точные typed child sources в том же snapshot.
// RESULT: FIXED — payload и все попытки остаются после удаления inventory, без эффектов.
// STATUS: FIXED

func TestPassTraceRetainsTypedChildInstructionAndEveryAttempt(t *testing.T) {
	ctx := context.Background()
	s, intent, child, input := childDeliveryFixture(t)
	delivery, err := s.PrepareEphemeralChildDelivery(ctx, intent.ID, input, childAllow)
	mustNil(t, err)
	now := time.Now().UTC()
	ticket, err := s.ReserveActorDeliveryAttempt(ctx, delivery.ID, now, now.Add(time.Second), allow)
	mustNil(t, err)
	mustNil(t, s.RecordActorDeliveryOutcome(ctx, ticket, "unconfirmed", "Receipt response lost", now, allow))
	_, err = s.ReserveActorDeliveryAttempt(ctx, delivery.ID, now.Add(time.Hour), now.Add(time.Hour+time.Second), allow)
	mustNil(t, err)
	attempts, err := s.GetActorDeliveryHistory(ctx, delivery.ID)
	mustNil(t, err)
	if len(attempts) != 2 || attempts[0].Attempt.Mode != "send" || attempts[1].Attempt.Mode != "probe" {
		t.Fatalf("fixture lost ambiguous send and pending probe: %+v", attempts)
	}
	before := databaseRows(t, s.db)
	trace, err := s.GetSwarmPassTrace(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Pass)
	mustNil(t, err)
	if len(trace.Steps) != 1 || trace.Steps[0].Children == nil {
		t.Fatal("trace lost child group")
	}
	got := trace.Steps[0].ChildInstructions
	want := []SwarmPassTraceDelivery{{Intent: delivery, Attempts: attempts}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace dropped original child instruction or transport evidence: got %+v, want %+v", got, want)
	}
	if trace.Steps[0].Instruction != nil || trace.Steps[0].Children.Children[0].ID != child.ID || trace.Steps[0].Children.Children[0].State != "running" || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("trace relabelled transport as completion or mutated history")
	}
	_, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision+1, "success", "Independent report retained", childAllow)
	mustNil(t, err)
	// Только отсутствие inventory в тесте; это не acceptance физического cleanup.
	_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid=?`, child.Executor.ActorUID)
	mustNil(t, err)
	before = databaseRows(t, s.db)
	trace, err = s.GetSwarmPassTrace(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Pass)
	mustNil(t, err)
	if !reflect.DeepEqual(trace.Steps[0].ChildInstructions, want) || trace.Steps[0].Children.Children[0].Report != "Independent report retained" || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("retained child instruction depended on live inventory or caused reconciliation")
	}
}

func TestPassTraceDoesNotInventUnpreparedChildInstruction(t *testing.T) {
	ctx := context.Background()
	s, intent, child, _ := childDeliveryFixture(t)
	before := databaseRows(t, s.db)
	trace, err := s.GetSwarmPassTrace(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Pass)
	mustNil(t, err)
	if len(trace.Steps) != 1 || trace.Steps[0].Children == nil || trace.Steps[0].Children.Children[0] != child || len(trace.Steps[0].ChildInstructions) != 0 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("trace invented preparation, lost pending child or mutated history")
	}
}
