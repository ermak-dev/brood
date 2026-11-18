package store

// FEATURE: Схема 59 добавляет eval, не выводя его из старых проходов.
// SPEC: Frozen58 rows неизменны; новая история пуста при upgrade;
// fixture59 содержит настоящие applied и stopped решения Store API.
// IMPLEMENTATION: Только пустые eval-таблицы при upgrade; fixture штатным генератором.
// RESULT: IMPLEMENTED — frozen26–58 не менялись, migration suite зелёный.
// STATUS: DONE

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestSwarmEvalMigration58PreservesRowsWithoutInventingBindings(t *testing.T) {
	path := loadSchemaFixture(t, 58)
	raw := rawDatabase(t, path)
	before := databaseRows(t, raw)
	before["swarm_eval_bindings"] = nil
	before["swarm_eval_replays"] = nil
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(withEmptyPolicyReviewTables(before), databaseRows(t, s.db)) {
		t.Fatal("eval migration rewrote history or invented eval binding/replay")
	}
	requireIntact(t, s.db)
}

func seedSwarmEvalFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	const project = "fixture-eval"
	seedProject(t, s, project, project)
	mustNil(t, s.SetProjectRepository(ctx, project, startOrigin))
	publish(t, s, project, map[string]string{"approve": startApprove, "mismatch": strings.Replace(strings.Replace(startApprove, "name: approve", "name: mismatch", 1), "label: Approve", "label: Different", 1)})
	for _, definition := range []string{"approve", "mismatch"} {
		id := "fixture-eval-" + definition
		mustNil(t, s.CreateSwarm(ctx, Swarm{ID: id, ProjectID: project, OwnerID: "root", Name: id, AtespaceName: id, AtespaceUID: ptr("native-" + id), State: "active"}))
		_, err := s.CreateSwarmEvalBinding(ctx, SwarmEvalBindingInput{CandidateSwarmID: id, ReferenceSwarmID: "one", ReferencePass: 1, LauncherID: "root", Definition: definition, Model: "fixture-eval-model"}, evalAuthorize)
		mustNil(t, err)
		_, err = s.StartSwarmEval(ctx, id, "root", evalAuthorize)
		mustNil(t, err)
		requests, err := s.SwarmRequests(ctx, id)
		mustNil(t, err)
		if len(requests) != 1 {
			t.Fatalf("fixture eval request count: %d", len(requests))
		}
		decision, err := s.ApplySwarmEvalReplay(ctx, SwarmEvalReplayInput{CandidateSwarmID: id, RequestID: requests[0].ID, LauncherID: "root"}, evalAuthorize, nil)
		mustNil(t, err)
		want := "applied"
		if definition == "mismatch" {
			want = "stopped"
		}
		if decision.State != want {
			t.Fatalf("fixture decision: %+v", decision)
		}
	}
}

func TestSwarmEvalFrozen59RetainsRealReplayAndStop(t *testing.T) {
	path := loadSchemaFixture(t, 59)
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	for _, definition := range []string{"approve", "mismatch"} {
		id := "fixture-eval-" + definition
		b, err := s.GetSwarmEvalBinding(context.Background(), id)
		mustNil(t, err)
		if !b.NonProduction || b.ReferencePass != 1 || b.Model != "fixture-eval-model" {
			t.Fatalf("lost fixture binding: %+v", b)
		}
		requests, err := s.SwarmRequests(context.Background(), id)
		mustNil(t, err)
		if len(requests) != 1 {
			t.Fatal("lost fixture request")
		}
		d, err := s.GetSwarmEvalReplay(context.Background(), id, requests[0].ID)
		mustNil(t, err)
		if definition == "approve" && (d.State != "applied" || d.Answer == nil || d.Answer.RecordedBy != "u" || requests[0].AnsweredBy != "root") {
			t.Fatalf("lost real replay: %+v", d)
		}
		if definition == "mismatch" && (d.State != "stopped" || d.Reason == "" || d.Answer != nil) {
			t.Fatalf("lost durable stop: %+v", d)
		}
	}
	requireIntact(t, s.db)
}
