package store

import (
	"context"
	"testing"
)

func seedEphemeralChildFailureFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	_, err := s.StartSwarm(ctx, SwarmStart{SwarmID: "two", UserID: "u", Definition: "build-first", Goal: "Retain a failed unregistered job for future migrations.", Commit: startCommit}, allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, "two", "fixture-failure-parent-attempt", "swarm:two")
	mustNil(t, err)
	mustNil(t, s.RecordActorStepAttestation(ctx, attempt, startOrigin, startCommit))
	op, err := s.CurrentActorOperation(ctx, attempt.Actor.UID)
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	parent := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "two", SwarmID: "two", AtespaceUID: attempt.Actor.AtespaceUID, ActorUID: attempt.Actor.UID, Incarnation: attempt.Actor.Incarnation}, Pass: attempt.Pass, Sequence: attempt.Sequence, RequestID: attempt.RequestID}
	job, err := s.EnqueueEphemeralChild(ctx, parent, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, "fixture-failed-unregistered-child", "No executor should be invented for this fixture job.", "pool", childAllow)
	mustNil(t, err)
	mustNil(t, s.RecordEphemeralChildFailure(ctx, job.ID, "Fixture server rejected this job before registration.", childAllow))
}

// FEATURE: Frozen55 содержит настоящий pre-registration failure для будущих миграций.
// SPEC: Отказ создан Store API, не INSERT; нет synthetic child, allocation или proof.
func TestEphemeralFailureCurrentFixtureRetainsRealDecision(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 55))
	defer db.Close()
	var reason, failedAt string
	var children, allocations int
	err := db.QueryRow(`SELECT f.reason,f.failed_at,
 (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=e.swarm_id AND c.parent_sequence=e.parent_sequence AND c.request_id=e.request_id),
 (SELECT count(*) FROM ephemeral_child_allocations a WHERE a.intent_id=e.id)
 FROM ephemeral_child_failures f JOIN ephemeral_child_intents e ON e.id=f.intent_id WHERE e.request_id='fixture-failed-unregistered-child'`).Scan(&reason, &failedAt, &children, &allocations)
	if err != nil {
		t.Fatalf("current schema55 fixture lost its real retained failure: %v", err)
	}
	if reason != "Fixture server rejected this job before registration." || failedAt == "" || children != 0 || allocations != 0 {
		t.Fatalf("failure fixture fabricated execution evidence: %q %q children=%d allocations=%d", reason, failedAt, children, allocations)
	}
}
