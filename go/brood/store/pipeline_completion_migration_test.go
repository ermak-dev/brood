package store

// FEATURE: Журнал completion intent не переинтерпретирует прежние исходы (#290).
// SPEC: Миграция сохраняет каждую строку schema38; новый журнал пуст, Open повторяем.

import (
	"context"
	"reflect"
	"testing"
)

func TestCompletionIntentMigrationPreservesAllLegacyRows(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 38)
	raw := rawDatabase(t, path)
	before := withEmptyDeliveryAndRequestContextTables(databaseRows(t, raw))
	before["pipeline_completion_intents"] = nil
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
		if after := databaseRows(t, s.db); !reflect.DeepEqual(after, before) {
			t.Fatal("migration changed history or invented completion intents")
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
}
