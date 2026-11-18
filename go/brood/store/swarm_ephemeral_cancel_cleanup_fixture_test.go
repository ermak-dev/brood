package store

import (
	"context"
	"testing"
)

// Новый fixture61 создаёт cancel_cleanup через публичные Store API, не SQL seed.
func seedEphemeralChildCancelCleanupFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	swarmID := "fixture-cancel-cleanup"
	mustNil(t, s.CreateSwarm(ctx, Swarm{ID: swarmID, ProjectID: "two", OwnerID: "u", Name: swarmID, AtespaceName: swarmID, AtespaceUID: ptr("native-" + swarmID), State: "active"}))
	addEphemeralCapacity(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("fixture-cancel-parent", swarmID, 501))
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "fixture-cancel-parent-start", "start")
	mustNil(t, err)
	a = lifecycleBind(t, s, a, "fixture-cancel-parent-start-task")
	a = lifecycleConfirm(t, s, a, "running")
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: swarmID, Role: "build", ActorUID: a.UID}))
	_, err = s.StartSwarm(ctx, SwarmStart{SwarmID: swarmID, UserID: "u", Definition: "build-first", Goal: "Retain no-instruction cancellation provenance.", Commit: startCommit}, allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, swarmID, "fixture-cancel-parent-attempt", "swarm:"+swarmID)
	mustNil(t, err)
	mustNil(t, s.RecordActorStepAttestation(ctx, attempt, startOrigin, startCommit))
	op, err := s.CurrentActorOperation(ctx, a.UID)
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	parent := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "two", SwarmID: swarmID, AtespaceUID: a.AtespaceUID, ActorUID: a.UID, Incarnation: a.Incarnation}, Pass: attempt.Pass, Sequence: attempt.Sequence, RequestID: attempt.RequestID}
	job, err := s.EnqueueEphemeralChild(ctx, parent, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, "fixture-cancelled-child", "This registered child must never receive an instruction.", "pool", childAllow)
	mustNil(t, err)
	addEphemeralCapacity(t, s)
	allocation, err := s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation("fixture-cancelled-worker", 502), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	_, err = s.ReserveEphemeralChildCreate(ctx, job.ID, childAllow)
	mustNil(t, err)
	a, err = s.GetManagedActor(ctx, allocation.ActorUID)
	mustNil(t, err)
	a = lifecycleBind(t, s, a, "fixture-cancelled-worker-create-task")
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.BeginEphemeralChildStart(ctx, job.ID, childAllow)
	mustNil(t, err)
	a, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "start", a.Revision, childAllow)
	mustNil(t, err)
	a = lifecycleBind(t, s, a, "fixture-cancelled-worker-start-task")
	lifecycleConfirm(t, s, a, "running")
	attestForRegistration(t, s, job)
	child, err := s.RegisterEphemeralChild(ctx, job.ID, childAllow)
	mustNil(t, err)
	_, err = s.CancelSwarmChild(ctx, parent, child.RequestID, child.Revision, childAllow)
	mustNil(t, err)
	_, err = s.JoinSwarmChildren(ctx, parent, childAllow)
	mustNil(t, err)
	_, err = s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
	mustNil(t, err)
}

func TestEphemeralCancelCleanupOfficial61Fixture(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 61))
	defer db.Close()
	var count int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM ephemeral_child_lifecycles l JOIN ephemeral_child_intents i ON i.id=l.intent_id JOIN swarm_child_tasks c ON c.swarm_id=i.swarm_id AND c.parent_sequence=i.parent_sequence AND c.request_id=i.request_id WHERE l.kind='cancel_cleanup' AND c.state='cancelled' AND c.revision=2 AND c.finished_at IS NOT NULL AND c.report IS NULL AND c.outcome IS NULL AND c.delivery='' AND NOT EXISTS(SELECT 1 FROM actor_deliveries d WHERE d.source_kind='child' AND d.source_id=c.id)`).Scan(&count))
	if count != 1 {
		t.Fatal("fixture lacks real no-execution cancellation cleanup")
	}
}
