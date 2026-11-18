package store

// FEATURE: Старые проходы явно legacy empty, а не сегодняшняя project policy (#290).
// SPEC: До миграции вычисляем полный ожидаемый набор ключей из прежних шагов;
// после повторного Open каждая прежняя строка и точный набор markers сохраняются.

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func legacyPassPolicyRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT quote(st.swarm_id)||','||quote(st.sequence)||','||quote(sw.project_id)||',NULL,1' FROM swarm_steps st JOIN swarms sw ON sw.id=st.swarm_id WHERE st.goal IS NOT NULL ORDER BY st.rowid`)
	mustNil(t, err)
	defer rows.Close()
	var result []string
	for rows.Next() {
		var value string
		mustNil(t, rows.Scan(&value))
		result = append(result, value)
	}
	mustNil(t, rows.Err())
	return result
}

func TestPassPolicyMigrationPreservesOldRowsAndPinsLegacyEmpty(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 37)
	raw := rawDatabase(t, path)
	before := withEmptyDeliveryAndRequestContextTables(databaseRows(t, raw))
	want := legacyPassPolicyRows(t, raw)
	if len(want) == 0 {
		t.Fatal("fixture contains no existing pass")
	}
	before["swarm_pass_policies"] = want
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
			t.Fatal("migration changed old rows or created incorrect legacy snapshots")
		}
		pin, err := s.SwarmStepWorkflowPolicy(ctx, "one", 1)
		mustNil(t, err)
		if !pin.LegacyEmpty || pin.Revision != 0 || len(pin.Rules) != 0 {
			t.Fatalf("legacy pass inherited current project configuration: %+v", pin)
		}
		current, err := s.ProjectWorkflowPolicy(ctx, "one")
		mustNil(t, err)
		if current.Revision != 2 {
			t.Fatal("migration changed project default")
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
}
