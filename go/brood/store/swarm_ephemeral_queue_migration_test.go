package store

import (
	"context"
	"maps"
	"reflect"
	"testing"
)

func withEmptyEphemeralChildIntents(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	out["ephemeral_child_intents"] = nil
	return withEmptyEphemeralAllocations(out)
}

func seedEphemeralChildQueueFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	request := managedRequest("fixture-queue-parent", "two", 305)
	pool, err := s.GetManagedPool(ctx, "pool")
	mustNil(t, err)
	pool.MaxActors++
	pool.Resources.CPUMillicores += request.Resources.CPUMillicores
	pool.Resources.MemoryBytes += request.Resources.MemoryBytes
	pool.Resources.DiskBytes += request.Resources.DiskBytes
	_, err = s.UpdateManagedPool(ctx, pool, pool.Revision)
	mustNil(t, err)
	_, err = s.ReserveManagedActor(ctx, request)
	mustNil(t, err)
	setActor(t, s, request.UID, "phase='running',stage='done',operation='start'")
	mustNil(t, s.AddMember(ctx, "two", "u"))
	mustNil(t, s.SetProjectRepository(ctx, "two", startOrigin))
	publish(t, s, "two", map[string]string{"build-first": startBuildFirst})
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "two", Role: "build", ActorUID: request.UID}))
	_, err = s.StartSwarm(ctx, SwarmStart{SwarmID: "two", UserID: "u", Definition: "build-first", Goal: "Retain pending ephemeral queue work.", Commit: startCommit}, allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, "two", "fixture-queue-parent-attempt", "swarm:two")
	mustNil(t, err)
	mustNil(t, s.RecordActorStepAttestation(ctx, attempt, startOrigin, startCommit))
	op, err := s.CurrentActorOperation(ctx, request.UID)
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	parent := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "two", SwarmID: "two", AtespaceUID: "native-two", ActorUID: request.UID, Incarnation: 1}, Pass: 1, Sequence: 1, RequestID: "fixture-queue-parent-attempt"}
	intent, err := s.EnqueueEphemeralChild(ctx, parent, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, "fixture-pending-child", "Retained queued child payload.", "pool", childAllow)
	mustNil(t, err)
	addEphemeralCapacity(t, s)
	_, err = s.AllocateEphemeralChild(ctx, intent.ID, ephemeralReservation("fixture-allocated-worker", 306), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
}

func TestEphemeralChildQueueMigrationPreservesAllVersion46History(t *testing.T) {
	path := loadSchemaFixture(t, 46)
	raw := rawDatabase(t, path)
	before := withEmptyEphemeralChildIntents(databaseRows(t, raw))
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("queue migration changed prior actor classes, pass contexts or operation history")
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM managed_actors WHERE actor_class='ephemeral'`).Scan(&count))
	if count == 0 {
		t.Fatal("queue migration lost retained ephemeral inventory")
	}
}

func TestEphemeralChildQueueCurrentFrozenFixtureCarriesPendingWork(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 47))
	var count int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM ephemeral_child_intents`).Scan(&count))
	if count == 0 {
		t.Fatal("frozen current fixture contains no queued work for future migrations")
	}
}
