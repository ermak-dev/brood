package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

// FEATURE: Старые allocation получают только legacy hold, не разрешение create.
// SPEC: Все прежние поля сохраняются; queued возможен только у нового allocation.
// IMPLEMENTATION: Миграция 51→52 явно удерживает старые allocation как legacy без send/receipt.
// RESULT: IMPLEMENTED — прежние строки сохранены, legacy нельзя перевести в разрешение.
// STATUS: DONE
func withLegacyCreateGrants(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := databaseRows(t, db)
	out["ephemeral_child_create_grants"] = nil
	out["ephemeral_child_result_seals"] = nil
	out["ephemeral_child_lifecycles"] = nil
	out["ephemeral_child_failures"] = nil
	out["owner_managed_actor_lineage"] = nil
	out["ephemeral_child_executor_failures"] = nil
	out["swarm_eval_bindings"] = nil
	out["swarm_eval_replays"] = nil
	rows, err := db.Query(`SELECT quote(intent_id)||','||quote('legacy')||','||quote(created_at)||','||quote('') FROM ephemeral_child_allocations ORDER BY rowid`)
	mustNil(t, err)
	defer rows.Close()
	for rows.Next() {
		var row string
		mustNil(t, rows.Scan(&row))
		out["ephemeral_child_create_grants"] = append(out["ephemeral_child_create_grants"], row)
	}
	mustNil(t, rows.Err())
	return withEmptyPolicyReviewTables(out)
}

func TestEphemeralCreateMigrationLegacyNeverFresh(t *testing.T) {
	for _, version := range []int{48, 49, 50, 51} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx := context.Background()
			path := loadSchemaFixture(t, version)
			raw := rawDatabase(t, path)
			before := withLegacyCreateGrants(t, raw)
			mustNil(t, raw.Close())
			s, err := Open(ctx, path)
			mustNil(t, err)
			defer s.Close()
			if version == 48 {
				before["ephemeral_child_attestations"] = nil
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("migration changed prior rows or did not explicitly hold old allocation")
			}
			var intentID string
			mustNil(t, s.db.QueryRow(`SELECT intent_id FROM ephemeral_child_allocations LIMIT 1`).Scan(&intentID))
			// Даже если прежний инвентарь выглядит как неотправленный create, историю не оживляем.
			setActor(t, s, "fixture-allocated-worker", "phase='provisioning',stage='create',operation='create'")
			held := databaseRows(t, s.db)
			grant, err := s.GetEphemeralChildCreateGrant(ctx, intentID)
			mustNil(t, err)
			if grant.State != "legacy" || !grant.Replay || grant.ReservedAt != "" {
				t.Fatal("migration fabricated create receipt/permission")
			}
			for range 2 {
				v, err := s.ReserveEphemeralChildCreate(ctx, intentID, childAllow)
				if err == nil || v != (EphemeralChildCreateGrant{}) {
					t.Fatal("legacy allocation obtained fresh physical permission")
				}
				if err := s.CheckEphemeralChildCreate(ctx, intentID, childAllow); err == nil {
					t.Fatal("legacy passed reserved check")
				}
			}
			for _, query := range []string{
				`UPDATE ephemeral_child_create_grants SET state='queued' WHERE intent_id=?`,
				`UPDATE ephemeral_child_create_grants SET state='reserved',reserved_at='now' WHERE intent_id=?`,
				`DELETE FROM ephemeral_child_create_grants WHERE intent_id=?`,
				`INSERT OR REPLACE INTO ephemeral_child_create_grants SELECT * FROM ephemeral_child_create_grants WHERE intent_id=?`,
			} {
				if _, err := s.db.Exec(query, intentID); err == nil {
					t.Fatal("legacy SQL hold rewritten")
				}
			}
			if !reflect.DeepEqual(held, databaseRows(t, s.db)) {
				t.Fatal("legacy refusal mutated history")
			}
			requireIntact(t, s.db)
		})
	}
}
