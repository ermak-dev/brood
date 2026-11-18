package store

import (
	"context"
	"reflect"
	"testing"
)

// FEATURE: Схема 50 разрешает proof-bound конверсию без переписывания истории.
// SPEC: Миграция 49→50 меняет только две ограды; fixture несёт pending child.
func TestEphemeralRegistrationMigrationPreservesExact49Rows(t *testing.T) {
	path := loadSchemaFixture(t, 49)
	raw := rawDatabase(t, path)
	before := withLegacyCreateGrants(t, raw)
	objects := schemaObjects(t, raw)
	conn, err := raw.Conn(context.Background())
	mustNil(t, err)
	mustNil(t, migrateStep(context.Background(), conn, schemaVersion, schemaMigrations))
	mustNil(t, conn.Close())
	current := schemaObjects(t, raw)
	changes := 0
	for i, object := range objects {
		if object != current[i] {
			changes++
		}
	}
	if len(objects) != len(current) || changes != 2 {
		t.Fatal("registration step changed objects beyond two guards")
	}
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("registration migration rewrote rows or fabricated children/proof")
	}
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion {
		t.Fatalf("migration reached %d, want %d", version, schemaVersion)
	}
	fresh := newTestStore(t)
	if !reflect.DeepEqual(schemaObjects(t, s.db), schemaObjects(t, fresh.db)) {
		t.Fatal("migrated and fresh catalogs differ")
	}
	requireIntact(t, s.db)
	var intentID string
	mustNil(t, s.db.QueryRow(`SELECT intent_id FROM ephemeral_child_allocations WHERE actor_uid='fixture-allocated-worker'`).Scan(&intentID))
	_, err = s.RegisterEphemeralChild(context.Background(), intentID, childAllow)
	mustNil(t, err)
}

func seedEphemeralChildRegistrationFixture(t *testing.T, s *Store) {
	t.Helper()
	var id string
	mustNil(t, s.db.QueryRow(`SELECT intent_id FROM ephemeral_child_allocations WHERE actor_uid='fixture-allocated-worker'`).Scan(&id))
	_, err := s.RegisterEphemeralChild(context.Background(), id, childAllow)
	mustNil(t, err)
}

func TestEphemeralRegistrationFrozen50RetainsAttestedPendingChild(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 50))
	var children, deliveries int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM swarm_child_tasks c JOIN ephemeral_child_intents e ON e.swarm_id=c.swarm_id AND e.parent_sequence=c.parent_sequence AND e.request_id=c.request_id JOIN ephemeral_child_allocations a ON a.intent_id=e.id JOIN ephemeral_child_attestations proof ON proof.intent_id=e.id WHERE a.actor_uid='fixture-allocated-worker' AND c.actor_uid=a.actor_uid AND c.actor_incarnation=a.actor_incarnation AND c.payload=e.payload AND c.role='' AND c.delivery='' AND c.state='pending' AND c.revision=1 AND proof.state='attested' AND proof.attested_origin=e.origin AND proof.attested_commit=e.source_commit`).Scan(&children))
	mustNil(t, db.QueryRow(`SELECT count(*) FROM actor_deliveries WHERE actor_uid='fixture-allocated-worker'`).Scan(&deliveries))
	if children != 1 || deliveries != 0 {
		t.Fatal("fixture lacks real attested pending child or delivered it")
	}
}
