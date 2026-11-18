package store

import (
	"context"
	"maps"
	"reflect"
	"testing"
)

func withEmptyEphemeralAllocations(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	out["ephemeral_child_allocations"] = nil
	return withEmptyEphemeralAttestations(out)
}

func TestEphemeralAllocationMigrationKeepsQueuedVersion47WorkUnallocated(t *testing.T) {
	path := loadSchemaFixture(t, 47)
	raw := rawDatabase(t, path)
	before := withEmptyEphemeralAllocations(databaseRows(t, raw))
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("allocation migration assigned workers or changed prior queue/history")
	}
	queued, err := s.QueuedEphemeralChildIntents(context.Background(), 0)
	mustNil(t, err)
	if len(queued) == 0 {
		t.Fatal("migration lost pending queued work")
	}
}

func TestEphemeralAllocationFrozenCurrentFixtureCarriesExactWorkerBinding(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 48))
	var count int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM ephemeral_child_allocations`).Scan(&count))
	if count == 0 {
		t.Fatal("frozen current fixture has no worker binding to preserve")
	}
}
