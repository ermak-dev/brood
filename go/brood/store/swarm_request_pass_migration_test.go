package store

import (
	"context"
	"maps"
	"reflect"
	"testing"
)

func withEmptyRequestContextAndPersistentActorClass(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	out["swarm_request_pass_contexts"] = nil
	return withEmptyEphemeralChildIntents(withPersistentActorClass(out))
}

func TestRequestPassContextMigrationPreservesLegacyUnscopedHistory(t *testing.T) {
	path := loadSchemaFixture(t, 44)
	old := rawDatabase(t, path)
	before := withEmptyEphemeralChildIntents(withPersistentActorClass(databaseRows(t, old)))
	mustNil(t, old.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	after := databaseRows(t, s.db)
	if rows, exists := after["swarm_request_pass_contexts"]; !exists || len(rows) != 0 {
		t.Fatal("migration assigned old actor questions to inferred passes")
	}
	delete(after, "swarm_request_pass_contexts")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("context migration changed existing source, request, delivery or policy history")
	}
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion {
		t.Fatalf("context migration reached version %d, want%d", version, schemaVersion)
	}
}

func TestRequestPassContextCurrentFrozenFixtureCarriesExplicitHistory(t *testing.T) {
	path := loadSchemaFixture(t, 45)
	db := rawDatabase(t, path)
	var count int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM swarm_request_pass_contexts`).Scan(&count))
	if count == 0 {
		t.Fatal("frozen current fixture carries no pass context to preserve in future migrations")
	}
}
