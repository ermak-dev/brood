package store

// FEATURE: Миграция result journal не выдумывает наблюдения и evidence (#290).
// SPEC: Все строки schema39 сохраняются; новые таблицы пусты, Open повторяем.

import (
	"context"
	"reflect"
	"testing"
)

func TestPipelineResultMigrationPreservesHistory(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 39)
	raw := rawDatabase(t, path)
	before := withEmptyDeliveryAndRequestContextTables(databaseRows(t, raw))
	before["pipeline_result_observations"] = nil
	before["pipeline_result_evidence"] = nil
	before["pipeline_completion_receipts"] = nil
	before["pipeline_completion_acknowledgements"] = nil
	before["pipeline_policy_candidates"] = nil
	before["pipeline_policy_confirmations"] = nil
	mustNil(t, raw.Close())
	for range 2 {
		s, err := Open(ctx, path)
		mustNil(t, err)
		if !reflect.DeepEqual(databaseRows(t, s.db), before) {
			t.Fatal("result migration changed history or invented evidence")
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
}
