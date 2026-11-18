package store

import (
	"context"
	"reflect"
	"testing"
)

func TestManagedNetworkMigrationPreservesLegacyRowsAndDefaults(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 35)
	raw := rawDatabase(t, path)
	before := databaseRows(t, raw)
	mustNil(t, raw.Close())
	s, err := Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	after := databaseRows(t, s.db)
	for table, want := range before {
		if got, exists := after[table]; !exists || !reflect.DeepEqual(got, want) {
			t.Fatalf("migration changed historical table %s", table)
		}
	}
	for table, rows := range after {
		if _, exists := before[table]; !exists && len(rows) != 0 {
			t.Fatalf("migration invented network records in %s", table)
		}
	}
	actors, err := s.ListManagedActors(ctx, "")
	mustNil(t, err)
	if len(actors) == 0 {
		t.Fatal("frozen fixture has no legacy actor")
	}
	wire := networkWire(t, "new-default.example")
	repository, err := s.ProjectRepository(ctx, "p")
	mustNil(t, err)
	_, err = s.SyncProject(ctx, "adm", "p", ProjectDeclaration{Name: "project", Repository: repository, ManagedNetwork: &wire}, func(context.Context) bool { return true })
	mustNil(t, err)
	for _, a := range actors {
		if a.NetworkPolicy != "" {
			t.Fatal("migration selected network for a legacy actor")
		}
		current, err := s.GetManagedActor(ctx, a.UID)
		mustNil(t, err)
		if current.NetworkPolicy != "" {
			t.Fatal("new default changed a historical actor without a snapshot")
		}
	}
	var snapshots int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM managed_actor_networks`).Scan(&snapshots))
	if snapshots != 0 {
		t.Fatal("reading legacy actors invented snapshots")
	}
	requireIntact(t, s.db)
}
