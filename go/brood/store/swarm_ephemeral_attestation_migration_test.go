package store

import (
	"context"
	"maps"
	"reflect"
	"testing"
)

// FEATURE: Схема 49 хранит аттестацию без регистрации ребёнка.
// SPEC: Миграция 48→49 добавляет пустой журнал и сохраняет все прежние строки.
// IMPLEMENTATION: Fixture 49 строится через Store API.
// RESULT: IMPLEMENTED
// STATUS: DONE
func withEmptyEphemeralAttestations(rows map[string][]string) map[string][]string {
	out := maps.Clone(rows)
	out["ephemeral_child_attestations"] = nil
	out["ephemeral_child_create_grants"] = nil
	out["ephemeral_child_result_seals"] = nil
	out["ephemeral_child_lifecycles"] = nil
	out["ephemeral_child_failures"] = nil
	out["owner_managed_actor_lineage"] = nil
	out["ephemeral_child_executor_failures"] = nil
	out["swarm_eval_bindings"] = nil
	out["swarm_eval_replays"] = nil
	return withEmptyPolicyReviewTables(out)
}

func TestEphemeralAttestationMigrationPreservesExactVersion48Rows(t *testing.T) {
	path := loadSchemaFixture(t, 48)
	raw := rawDatabase(t, path)
	before := withLegacyCreateGrants(t, raw)
	before["ephemeral_child_attestations"] = nil
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("attestation migration changed old history or invented proof/permission")
	}
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion {
		t.Fatalf("migration reached %d, want %d", version, schemaVersion)
	}
	requireIntact(t, s.db)
}

func seedEphemeralChildAttestationFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	var intentID string
	mustNil(t, s.db.QueryRow(`SELECT intent_id FROM ephemeral_child_allocations WHERE actor_uid='fixture-allocated-worker'`).Scan(&intentID))
	intent, err := s.GetEphemeralChildIntent(ctx, intentID)
	mustNil(t, err)
	grant, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
	mustNil(t, err)
	if grant.Replay || grant.State != "reserved" {
		t.Fatal("fixture did not consume a fresh create grant before simulated readiness")
	}
	// Провайдер не запускается: fixture моделирует его ready-переход.
	setActor(t, s, "fixture-allocated-worker", "phase='running',stage='done',operation='start'")
	_, err = s.ReserveEphemeralChildAttestation(ctx, intent.ID, "fixture-attestation", childAllow)
	mustNil(t, err)
	mustNil(t, s.RecordEphemeralChildAttestation(ctx, intent.ID, "fixture-attestation", intent.Origin, intent.SourceCommit, childAllow))
}

func TestEphemeralAttestationFrozen49RetainsProofWithoutChild(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 49))
	var proofs, children int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM ephemeral_child_attestations e JOIN ephemeral_child_allocations a ON a.intent_id=e.intent_id WHERE e.state='attested' AND e.attested_origin<>'' AND e.attested_commit<>'' AND a.actor_uid='fixture-allocated-worker'`).Scan(&proofs))
	mustNil(t, db.QueryRow(`SELECT count(*) FROM swarm_child_tasks WHERE actor_uid='fixture-allocated-worker'`).Scan(&children))
	if proofs != 1 || children != 0 {
		t.Fatal("fixture lacks representative retained pre-registration proof")
	}
}
