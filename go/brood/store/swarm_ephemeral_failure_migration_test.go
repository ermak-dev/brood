package store

import (
	"context"
	"reflect"
	"testing"
)

// FEATURE: 54→55 добавляет только пустую историю серверных отказов.
// SPEC: Все строки frozen54 неизменны, failures не выводятся из inventory.
func TestEphemeralFailureMigrationEveryOldRow(t *testing.T) {
	// Проверяем именно новый шаг: прежние миграции добавляли столбцы и историю.
	for _, version := range []int{54} {
		t.Run(schemaFixturePath(version), func(t *testing.T) {
			path := loadSchemaFixture(t, version)
			raw := rawDatabase(t, path)
			before := withEmptyPolicyReviewTables(databaseRows(t, raw))
			mustNil(t, raw.Close())
			s, err := Open(context.Background(), path)
			mustNil(t, err)
			defer s.Close()
			before["ephemeral_child_failures"] = nil
			before["owner_managed_actor_lineage"] = nil
			before["ephemeral_child_executor_failures"] = nil
			before["swarm_eval_bindings"] = nil
			before["swarm_eval_replays"] = nil
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatalf("migration rewrote schema%d rows or inferred failures", version)
			}
			var n int
			mustNil(t, s.db.QueryRow(`SELECT count(*) FROM ephemeral_child_failures`).Scan(&n))
			if n != 0 {
				t.Fatal("migration inferred legacy failures")
			}
			requireIntact(t, s.db)
		})
	}
}

func TestEphemeralFailureReopenRetainsFact(t *testing.T) {
	ctx := context.Background()
	s, job := allocationFixture(t, 1)
	mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, "Retained local failure", childAllow))
	var path string
	mustNil(t, s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path))
	before := databaseRows(t, s.db)
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	if !reflect.DeepEqual(before, databaseRows(t, reopened.db)) {
		t.Fatal("reopen changed retained failure or fabricated proof")
	}
	j, err := reopened.GetSwarmChildJoin(ctx, job.Parent, childAllow)
	mustNil(t, err)
	if j.Evaluation.Open != 0 || j.Evaluation.State != "ready_fail" {
		t.Fatalf("reopen lost logical failure: %+v", j)
	}
}
