package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// Перестройка таблицы сохраняет непустую историю кандидатов и ответов;
// подтверждённый ответ не превращается в выполненный переход или review.
func TestPolicyProgressionMigrationPreservesAnsweredCandidate61(t *testing.T) {
	ctx := context.Background()
	source, candidate := policyConfirmationFixture(t)
	answer, err := source.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
	mustNil(t, err)

	frozen := rawDatabase(t, loadSchemaFixture(t, 61))
	rows, err := frozen.Query(`SELECT type,name,sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END,name`)
	mustNil(t, err)
	type object struct{ kind, name, ddl string }
	var objects []object
	for rows.Next() {
		var o object
		mustNil(t, rows.Scan(&o.kind, &o.name, &o.ddl))
		objects = append(objects, o)
	}
	mustNil(t, rows.Err())
	mustNil(t, rows.Close())

	path := filepath.Join(t.TempDir(), "answered-policy-v61.db")
	legacy := rawDatabase(t, path)
	for _, o := range objects {
		if o.kind != "table" {
			continue
		}
		_, err := legacy.Exec(o.ddl)
		mustNil(t, err)
		for _, row := range tableRows(t, source.db, o.name) {
			_, err := legacy.Exec(fmt.Sprintf("INSERT INTO %q VALUES(%s)", o.name, row))
			mustNil(t, err)
		}
	}
	for _, o := range objects {
		if o.kind != "table" {
			_, err := legacy.Exec(o.ddl)
			mustNil(t, err)
		}
	}
	_, err = legacy.Exec(`PRAGMA user_version=61`)
	mustNil(t, err)
	requireIntact(t, legacy)
	if !reflect.DeepEqual(schemaObjects(t, frozen), schemaObjects(t, legacy)) {
		t.Fatal("retained policy fixture is not the frozen schema61")
	}
	before := withEmptyPolicyReviewTables(databaseRows(t, legacy))
	if len(before["pipeline_policy_candidates"]) == 0 || len(before["pipeline_policy_confirmations"]) == 0 {
		t.Fatalf("migration fixture has no actual candidate and answer: candidates=%d, answers=%d", len(before["pipeline_policy_candidates"]), len(before["pipeline_policy_confirmations"]))
	}
	mustNil(t, legacy.Close())

	for range 2 {
		migrated, err := Open(ctx, path)
		mustNil(t, err)
		if !reflect.DeepEqual(before, databaseRows(t, migrated.db)) {
			t.Fatal("migration changed retained history or invented review")
		}
		gotCandidate, err := migrated.GetPipelinePolicyCandidateByID(ctx, "one", candidate.ID)
		mustNil(t, err)
		gotAnswer, err := migrated.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID)
		mustNil(t, err)
		if !reflect.DeepEqual(gotCandidate, candidate) || !reflect.DeepEqual(gotAnswer, answer) {
			t.Fatal("migration changed candidate material or exact answer")
		}
		step, err := migrated.GetSwarmStep(ctx, "one", 1)
		mustNil(t, err)
		if step.Outcome != nil {
			t.Fatal("retained answer applied the transition during migration")
		}
		requireIntact(t, migrated.db)
		mustNil(t, migrated.Close())
	}
}
