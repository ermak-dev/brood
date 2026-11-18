package store

import (
	"context"
	"maps"
	"reflect"
	"testing"
)

// Новые журналы при upgrade пусты. Прежние review claims не становятся evidence.
func withEmptyPolicyReviewTables(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	for _, table := range []string{"pipeline_policy_review_requirements", "pipeline_policy_review_assignments", "pipeline_policy_review_evidence", "project_ownership"} {
		if _, exists := out[table]; !exists {
			out[table] = nil
		}
	}
	return out
}

func TestPolicyProgressionMigrationPreservesEvery61RowWithoutInventingReview(t *testing.T) {
	path := loadSchemaFixture(t, 61)
	raw := rawDatabase(t, path)
	before := withEmptyPolicyReviewTables(databaseRows(t, raw))
	mustNil(t, raw.Close())
	for range 2 {
		s, err := Open(context.Background(), path)
		mustNil(t, err)
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("schema62 changed history or invented review")
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
}
