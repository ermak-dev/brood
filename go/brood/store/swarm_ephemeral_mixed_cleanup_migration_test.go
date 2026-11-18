package store

import (
	"context"
	"reflect"
	"testing"
)

// FEATURE: Preserving 59→60 меняет только cleanup insert guard.
// SPEC: Все исторические rows, eval bindings, отказы, отчёты и grants неизменны;
// новый guard не создаёт provenance и не выводит permission из отсутствия данных.
func TestEphemeralMixedCleanupMigrationPreservesEvery59Row(t *testing.T) {
	path := loadSchemaFixture(t, 59)
	raw := rawDatabase(t, path)
	before := databaseRows(t, raw)
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion || !reflect.DeepEqual(withEmptyPolicyReviewTables(before), databaseRows(t, s.db)) {
		t.Fatalf("cleanup guard migration did not preserve schema59 rows: version=%d", version)
	}
	requireIntact(t, s.db)
}
