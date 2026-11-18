package store

// Миграция сохраняет непустую историю кандидатов, не выдумывая ответов.

import (
	"context"
	"reflect"
	"testing"
)

func TestPolicyConfirmationMigrationRetainsRealCandidateWithoutInventedAnswer(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 42)
	raw := rawDatabase(t, path)
	old := &Store{db: raw}
	_, err := old.SetProjectWorkflowPolicy(ctx, "root", "one", 2, policyRules(), allow)
	mustNil(t, err)
	// Подготовка visit использует только поля версии 42. Нынешний public StartSwarm
	// проверяет и более новые child tables; candidate всё ещё создаёт настоящий API.
	mustNil(t, old.WithTx(ctx, func(ctx context.Context) error {
		swarm, err := old.GetSwarm(ctx, "one")
		if err != nil {
			return err
		}
		in := SwarmStart{SwarmID: "one", UserID: "u", Definition: "approve", Goal: "Retain a real schema42 policy candidate.", Commit: startCommit}
		plan, err := old.planPass(ctx, swarm, in)
		if err != nil {
			return err
		}
		return old.beginPass(ctx, swarm.ID, in.Goal, plan)
	}))
	var sequence int64
	mustNil(t, raw.QueryRow(`SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id='one'`).Scan(&sequence))
	candidate, err := old.RecordPipelinePolicyCandidate(ctx, "one", sequence, "entry", "candidate-before-43")
	mustNil(t, err)
	before := withEmptyDeliveryAndRequestContextTables(databaseRows(t, raw))
	if len(before["pipeline_policy_candidates"]) != 1 {
		t.Fatal("migration fixture carries no real candidate")
	}
	before["pipeline_policy_confirmations"] = nil
	mustNil(t, raw.Close())
	for range 2 {
		s, err := Open(ctx, path)
		mustNil(t, err)
		if !reflect.DeepEqual(databaseRows(t, s.db), before) {
			t.Fatal("confirmation migration changed history or invented approval")
		}
		got, err := s.GetPipelinePolicyCandidate(ctx, "one", sequence, "entry")
		mustNil(t, err)
		if !reflect.DeepEqual(got, candidate) {
			t.Fatal("candidate was reinterpreted during migration")
		}
		requireIntact(t, s.db)
		mustNil(t, s.Close())
	}
}

func TestPolicyConfirmationSurvivesActualCloseOpenWithoutApplication(t *testing.T) {
	ctx := context.Background()
	s, candidate := policyConfirmationFixture(t)
	first, err := s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
	mustNil(t, err)
	var sequence int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	got, err := reopened.GetPipelinePolicyConfirmation(ctx, "one", candidate.ID)
	mustNil(t, err)
	if !reflect.DeepEqual(got, first) || stepCount(t, reopened) != 1 || stepOf(t, reopened, 1).State != "inflight" {
		t.Fatal("close/open rewrote confirmation or applied the candidate")
	}
}
