package store

// Миграция не выдумывает решения старым переходам и сохраняет все прежние строки.

import (
	"context"
	"reflect"
	"testing"
)

func TestPolicyCandidateMigrationPreservesAllLegacyHistory(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 41)
	raw := rawDatabase(t, path)
	before := withEmptyDeliveryAndRequestContextTables(databaseRows(t, raw))
	before["pipeline_policy_candidates"] = nil
	before["pipeline_policy_confirmations"] = nil
	mustNil(t, raw.Close())
	for range 2 {
		s, err := Open(ctx, path)
		mustNil(t, err)
		if !reflect.DeepEqual(databaseRows(t, s.db), before) {
			t.Fatal("migration changed history or invented policy decisions")
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
}

func TestPolicyCandidateSurvivesActualCloseOpen(t *testing.T) {
	ctx := context.Background()
	s := policyPreviewStore(t, startBuildFirst, policyRules())
	candidate, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "entry", "restart-candidate")
	mustNil(t, err)
	var sequence int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	retained, err := reopened.GetPipelinePolicyCandidate(ctx, "one", 1, "entry")
	mustNil(t, err)
	if !reflect.DeepEqual(retained, candidate) {
		t.Fatal("restart rewrote candidate or decision")
	}
	if stepCount(t, reopened) != 1 || stepOf(t, reopened, 1).State != "pending" {
		t.Fatal("restart applied an unapplied candidate")
	}
}
