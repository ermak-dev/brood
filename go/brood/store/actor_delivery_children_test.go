package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// FEATURE: Отдельный bounded reader retained child deliveries (#292).
// SPEC: Только typed running child, cursor и лимит; confirmed/exhausted/terminal
// исключены, чтение не пишет и не является authority или разрешением эффекта.
// IMPLEMENTATION: Отдельный typed SQL snapshot с bounded cursor.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestPendingActorDeliveryChildrenPagesOnlyRetainedTypedChildren(t *testing.T) {
	ctx := context.Background()
	s, intent, _, in := childDeliveryFixture(t)
	delivery, err := s.PrepareEphemeralChildDelivery(ctx, intent.ID, in, childAllow)
	mustNil(t, err)
	for _, kind := range []string{"step", "answer"} {
		request, commit := "legacy-request", "legacy-commit"
		if kind == "answer" {
			request, commit = "", ""
		}
		_, err := s.db.Exec(`INSERT INTO actor_deliveries SELECT ?,swarm_id,?,source_id,?,?,actor_uid,incarnation,atespace_uid,journal_id,payload,probe_only,created_at FROM actor_deliveries WHERE id=?`, "legacy-"+kind, kind, request, commit, delivery.ID)
		mustNil(t, err)
	}
	before := databaseRows(t, s.db)
	got, err := s.PendingActorDeliveryChildren(ctx, "", 1)
	mustNil(t, err)
	if !reflect.DeepEqual(got, []ActorDeliveryCandidate{{ID: delivery.ID, SwarmID: in.SwarmID, SourceID: in.SourceID}}) {
		t.Fatalf("wrong typed candidates: %+v", got)
	}
	got, err = s.PendingActorDeliveryChildren(ctx, delivery.ID, 1)
	mustNil(t, err)
	if len(got) != 0 {
		t.Fatal("exclusive cursor repeated delivery")
	}
	for _, limit := range []int{-1, 0, 65} {
		if got, err := s.PendingActorDeliveryChildren(ctx, "", limit); err == nil || got != nil {
			t.Fatal("invalid page size accepted or returned partial data")
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("candidate reader mutated history")
	}
}

func TestPendingActorDeliveryChildrenExcludesConfirmedExhaustedAndCompleted(t *testing.T) {
	for _, scenario := range []string{"confirmed", "exhausted", "completed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, intent, child, in := childDeliveryFixture(t)
			delivery, err := s.PrepareEphemeralChildDelivery(ctx, intent.ID, in, childAllow)
			mustNil(t, err)
			now := time.Now().UTC()
			switch scenario {
			case "confirmed":
				ticket, err := s.ReserveActorDeliveryAttempt(ctx, delivery.ID, now, now.Add(time.Second), allow)
				mustNil(t, err)
				mustNil(t, s.RecordActorDeliveryOutcome(ctx, ticket, "delivered", "fixture receipt", now, allow))
			case "exhausted":
				for i := range MaxActorDeliveryAttempts {
					at := now.Add(time.Duration(i) * time.Hour)
					ticket, err := s.ReserveActorDeliveryAttempt(ctx, delivery.ID, at, at.Add(time.Second), allow)
					mustNil(t, err)
					mustNil(t, s.RecordActorDeliveryOutcome(ctx, ticket, "unconfirmed", "fixture unknown", at, allow))
				}
			case "completed":
				_, err := s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, 2, "success", "Independent retained report", childAllow)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			got, err := s.PendingActorDeliveryChildren(ctx, "", 64)
			mustNil(t, err)
			if len(got) != 0 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatalf("settled child remained a recovery candidate: %+v", got)
			}
		})
	}
}
