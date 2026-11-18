package store

import (
	"context"
	"maps"
	"reflect"
	"testing"
)

func withPersistentActorClass(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	var actors []string
	for _, row := range rows["managed_actors"] {
		actors = append(actors, row+",'persistent'")
	}
	out["managed_actors"] = actors
	return out
}

func seedManagedClassFixture(t *testing.T, s *Store, parent string) {
	t.Helper()
	ctx := context.Background()
	pool, err := s.GetManagedPool(ctx, "pool")
	mustNil(t, err)
	request := managedRequest("fixture-ephemeral-actor", "one", 304)
	pool.MaxActors++
	pool.Resources.CPUMillicores += request.Resources.CPUMillicores
	pool.Resources.MemoryBytes += request.Resources.MemoryBytes
	pool.Resources.DiskBytes += request.Resources.DiskBytes
	_, err = s.UpdateManagedPool(ctx, pool, pool.Revision)
	mustNil(t, err)
	request.Class, request.ParentUID = ActorClassEphemeral, parent
	_, err = s.reserveManagedActor(ctx, request)
	mustNil(t, err)
}

func TestManagedActorClassCurrentFixtureRetainsEphemeralInventory(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 46))
	var count int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM managed_actors WHERE actor_class='ephemeral'`).Scan(&count))
	if count == 0 {
		t.Fatal("current frozen fixture has no ephemeral class to preserve in future migrations")
	}
}

func TestManagedActorClassMigrationPreservesAllVersion45Rows(t *testing.T) {
	path := loadSchemaFixture(t, 45)
	raw := rawDatabase(t, path)
	before := withEmptyEphemeralChildIntents(withPersistentActorClass(databaseRows(t, raw)))
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("actor class migration changed historical fields instead of adding persistent default")
	}
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion {
		t.Fatalf("actor class migration reached version %d, want%d", version, schemaVersion)
	}
	actors, err := s.ListManagedActors(context.Background(), "")
	mustNil(t, err)
	if len(actors) == 0 {
		t.Fatal("fixture has no actor history to preserve")
	}
	for _, a := range actors {
		if a.Class != ActorClassPersistent {
			t.Fatal("migration invented ephemeral provenance for an old actor")
		}
	}
}
