package store

import (
	"context"
	"reflect"
	"testing"
)

// FEATURE: 62→63 сохраняет все строки и не выводит owner из старого членства.
// SPEC: Новая таблица grant пуста, прежние actor/policy/eval данные не меняются.
func TestProjectOwnershipMigrationDoesNotInferHistoricalOwners(t *testing.T) {
	path := loadSchemaFixture(t, 62)
	raw := rawDatabase(t, path)
	before := databaseRows(t, raw)
	mustNil(t, raw.Close())
	for range 2 {
		s, err := Open(context.Background(), path)
		mustNil(t, err)
		owner, err := s.ProjectOwnership(context.Background(), "one")
		mustNil(t, err)
		if owner.Revision != 0 || owner.OwnerID != "" {
			t.Fatal("migration inferred an owner from historical data")
		}
		after := databaseRows(t, s.db)
		if len(after["project_ownership"]) != 0 {
			t.Fatal("migration invented ownership grants")
		}
		delete(after, "project_ownership")
		if !reflect.DeepEqual(before, after) {
			t.Fatal("ownership migration changed historical rows")
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
}
