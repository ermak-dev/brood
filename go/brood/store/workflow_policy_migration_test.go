package store

// FEATURE: Миграция добавляет пустой каталог policy, не меняя старую историю (#290).
// SPEC: Все прежние строки и версии 26–36 остаются неизменными; повторный Open безопасен.

import (
	"context"
	"reflect"
	"testing"
)

func TestWorkflowPolicyMigrationPreservesLegacyRows(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 36)
	raw := rawDatabase(t, path)
	before := withPersistentActorClass(databaseRows(t, raw))
	before["swarm_pass_policies"] = legacyPassPolicyRows(t, raw)
	mustNil(t, raw.Close())
	for range 2 {
		s, err := Open(ctx, path)
		mustNil(t, err)
		after := databaseRows(t, s.db)
		for table, want := range before {
			if got, exists := after[table]; !exists || !reflect.DeepEqual(got, want) {
				t.Fatalf("migration changed historical table %s", table)
			}
		}
		for table, rows := range after {
			if _, exists := before[table]; !exists && len(rows) != 0 {
				t.Fatalf("migration invented policy records in %s", table)
			}
		}
		projects, err := s.db.Query(`SELECT id FROM projects`)
		mustNil(t, err)
		var ids []string
		for projects.Next() {
			var id string
			mustNil(t, projects.Scan(&id))
			ids = append(ids, id)
		}
		mustNil(t, projects.Err())
		mustNil(t, projects.Close())
		if len(ids) == 0 {
			t.Fatal("fixture has no legacy project")
		}
		for _, id := range ids {
			policy, err := s.ProjectWorkflowPolicy(ctx, id)
			mustNil(t, err)
			if policy.Revision != 0 || len(policy.Rules) != 0 {
				t.Fatal("migration assigned policy to a legacy project")
			}
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
}
