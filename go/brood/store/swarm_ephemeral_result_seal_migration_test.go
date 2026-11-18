package store

import (
	"context"
	"reflect"
	"testing"
)

// FEATURE: Миграция result seal сохраняет старую историю без cleanup authority.
// SPEC: 52→53 только добавляет пустую таблицу; fixture использует реальные отчёт и join.
func TestEphemeralResultSealMigrationPreservesEvery52Row(t *testing.T) {
	path := loadSchemaFixture(t, 52)
	raw := rawDatabase(t, path)
	before := withEmptyPolicyReviewTables(databaseRows(t, raw))
	before["ephemeral_child_result_seals"] = nil
	before["ephemeral_child_lifecycles"] = nil
	before["ephemeral_child_failures"] = nil
	before["owner_managed_actor_lineage"] = nil
	before["ephemeral_child_executor_failures"] = nil
	before["swarm_eval_bindings"] = nil
	before["swarm_eval_replays"] = nil
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("migration changed existing rows or fabricated external result evidence")
	}
	requireIntact(t, s.db)
}

func seedEphemeralChildResultSealFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	intent, err := s.GetEphemeralChildIntentForActor(ctx, "fixture-allocated-worker", 1)
	mustNil(t, err)
	var childID string
	mustNil(t, s.db.QueryRow(`SELECT id FROM swarm_child_tasks WHERE actor_uid='fixture-allocated-worker'`).Scan(&childID))
	work, err := s.SwarmChildWorkByID(ctx, childID)
	mustNil(t, err)
	_, err = s.CompleteSwarmChild(ctx, intent.Parent, work.Task.RequestID, work.Task.Executor, work.Task.Revision, "success", "Independent fixture result retained outside the ephemeral workspace.", childAllow)
	mustNil(t, err)
	_, err = s.JoinSwarmChildren(ctx, intent.Parent, childAllow)
	mustNil(t, err)
	mustNil(t, s.SealEphemeralChildResult(ctx, intent.ID, childAllow))
}
