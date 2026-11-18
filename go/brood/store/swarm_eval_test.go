package store

// FEATURE: Долговечная изоляция eval до создания акторов (#293).
// SPEC: Immutable binding фиксирует reference pass/source/goal и candidate
// definition/model; distinct project, no inventory including tombstones;
// FULL transaction, current local authorization on retries; ordinary start
// and all human answers refuse bound swarms. Eval starts once from binding.
// IMPLEMENTATION: Отдельная immutable binding, FULL admission и guard до policy branch.
// RESULT: IMPLEMENTED — normal/race и полный Store suite зелёные.
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func evalFixture(t *testing.T) (*Store, SwarmEvalBindingInput) {
	t.Helper()
	s := startStore(t)
	ctx := context.Background()
	_, err := startOne(s, "approve")
	mustNil(t, err)
	req := openNodeRequestOf(t, s, 1)
	_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: req.ID, UserID: "u", Outcome: "success", Response: "Original human notes"}, allow)
	mustNil(t, err)
	mustNil(t, s.SetProjectRepository(ctx, "two", startOrigin))
	mustNil(t, s.AddMember(ctx, "two", "u"))
	publish(t, s, "two", map[string]string{"approve": startApprove})
	return s, SwarmEvalBindingInput{CandidateSwarmID: "two", ReferenceSwarmID: "one", ReferencePass: 1, LauncherID: "u", Definition: "approve", Model: "selected-model-1"}
}

func evalAuthorize(ctx context.Context, action string, binding SwarmEvalBinding) error { return nil }

func TestSwarmEvalBindingPinsBeforeExecutionAndRefusesLivePaths(t *testing.T) {
	ctx := context.Background()
	s, in := evalFixture(t)
	binding, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	mustNil(t, err)
	if binding.Goal != startGoal || binding.SourceCommit != startCommit || binding.Origin != startOrigin || !binding.NonProduction || binding.Model != in.Model || binding.ReferencePass != 1 || binding.CandidateProjectID != "two" || binding.ReferenceProjectID != "one" || binding.Graph != startApprove {
		t.Fatalf("binding lost selection: %+v", binding)
	}
	again, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	mustNil(t, err)
	if !reflect.DeepEqual(binding, again) {
		t.Fatal("retry changed immutable binding")
	}
	_, err = s.StartSwarm(ctx, SwarmStart{SwarmID: "two", UserID: "u", Definition: "approve", Goal: startGoal, Commit: startCommit}, allow)
	if !errors.Is(err, ErrSwarmEvalConflict) {
		t.Fatalf("ordinary start: %v", err)
	}
	// Изменение каталога не меняет уже выбранный immutable graph.
	publish(t, s, "two", map[string]string{"approve": strings.Replace(startRelease, "name: release", "name: approve", 1)})
	started, err := s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
	mustNil(t, err)
	if started.Execution != "waiting" || string(started.Graph) != startApprove {
		t.Fatalf("start did not use pinned graph: %+v", started)
	}
	visit, err := s.GetSwarmStep(ctx, "two", 1)
	mustNil(t, err)
	if visit.Goal != startGoal || visit.SourceCommit != startCommit {
		t.Fatalf("source/goal changed: %+v", visit)
	}
	for _, requestID := range []string{"not-a-request", policyRequestPrefix + "not-a-policy"} {
		_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "two", RequestID: requestID, UserID: "u", Response: "yes"}, allow)
		if !errors.Is(err, ErrSwarmEvalConflict) {
			t.Fatalf("human answer %q: %v", requestID, err)
		}
	}
	_, err = s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
	if !errors.Is(err, ErrSwarmEvalConflict) {
		t.Fatalf("second pass: %v", err)
	}
	got, err := s.GetSwarmEvalBinding(ctx, "two")
	mustNil(t, err)
	if !reflect.DeepEqual(got, binding) {
		t.Fatal("start mutated binding")
	}
}

func TestSwarmEvalBindingReauthorizesAndRejectsChangedRetries(t *testing.T) {
	ctx := context.Background()
	s, in := evalFixture(t)
	_, err := s.CreateSwarmEvalBinding(ctx, in, nil)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("nil authority: %v", err)
	}
	count := 0
	auth := func(tx context.Context, action string, b SwarmEvalBinding) error {
		count++
		var full int
		mustNil(t, s.conn(tx).QueryRowContext(tx, `PRAGMA synchronous`).Scan(&full))
		if full != 2 {
			t.Fatalf("not FULL: %d", full)
		}
		_, err := s.GetSwarm(tx, b.ReferenceSwarmID)
		return err
	}
	_, err = s.CreateSwarmEvalBinding(ctx, in, auth)
	mustNil(t, err)
	_, err = s.CreateSwarmEvalBinding(ctx, in, auth)
	mustNil(t, err)
	if count != 2 {
		t.Fatalf("retry authority calls: %d", count)
	}
	changed := in
	changed.Model = "another-model"
	_, err = s.CreateSwarmEvalBinding(ctx, changed, auth)
	if !errors.Is(err, ErrSwarmEvalConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	denied := func(context.Context, string, SwarmEvalBinding) error { return ErrSwarmRequestDenied }
	_, err = s.CreateSwarmEvalBinding(ctx, in, denied)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("revoked retry: %v", err)
	}
	_, err = s.StartSwarmEval(ctx, "two", "u", denied)
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("revoked start: %v", err)
	}
	mustNil(t, s.WithTx(ctx, func(tx context.Context) error {
		_, err := s.CreateSwarmEvalBinding(tx, in, auth)
		if !errors.Is(err, ErrDurableTransactionNested) {
			t.Fatalf("nested binding: %v", err)
		}
		_, err = s.StartSwarmEval(tx, "two", "u", auth)
		if !errors.Is(err, ErrDurableTransactionNested) {
			t.Fatalf("nested start: %v", err)
		}
		return nil
	}))
}

func TestSwarmEvalBindingRejectsUnsafeCandidates(t *testing.T) {
	for _, variant := range []string{"actor", "tombstone", "started", "same-project", "foreign-origin", "reference-running", "invalid-model", "callback-actor"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			s, in := evalFixture(t)
			auth := evalAuthorize
			switch variant {
			case "actor", "tombstone":
				_, err := s.ReserveManagedActor(ctx, managedRequest("candidate", "two", 202))
				mustNil(t, err)
				if variant == "tombstone" {
					setActor(t, s, "candidate", "phase='deleted'")
				}
			case "started":
				_, err := s.StartSwarm(ctx, SwarmStart{SwarmID: "two", UserID: "u", Definition: "approve", Goal: startGoal, Commit: startCommit}, allow)
				mustNil(t, err)
			case "same-project":
				mustNil(t, s.CreateSwarm(ctx, Swarm{ID: "candidate", ProjectID: "one", OwnerID: "u", Name: "candidate", AtespaceName: "candidate", State: "active"}))
				in.CandidateSwarmID = "candidate"
			case "foreign-origin":
				mustNil(t, s.SetProjectRepository(ctx, "two", "https://git.example.org/other/repo.git"))
			case "reference-running":
				_, err := startOne(s, "approve")
				mustNil(t, err)
				in.ReferencePass = 2
			case "invalid-model":
				in.Model = "has spaces"
			case "callback-actor":
				auth = func(tx context.Context, _ string, _ SwarmEvalBinding) error {
					_, err := s.ReserveManagedActor(tx, managedRequest("candidate", "two", 202))
					return err
				}
			}
			_, err := s.CreateSwarmEvalBinding(ctx, in, auth)
			if err == nil {
				t.Fatal("unsafe binding accepted")
			}
			_, err = s.GetSwarmEvalBinding(ctx, in.CandidateSwarmID)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed binding retained: %v", err)
			}
		})
	}
}

func TestSwarmEvalBindingIgnoredInsertAndImmutableHistory(t *testing.T) {
	ctx := context.Background()
	s, in := evalFixture(t)
	_, err := s.db.Exec(`CREATE TRIGGER ignore_eval BEFORE INSERT ON swarm_eval_bindings BEGIN SELECT RAISE(IGNORE); END`)
	mustNil(t, err)
	_, err = s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	if err == nil {
		t.Fatal("ignored binding reported success")
	}
	_, err = s.db.Exec(`DROP TRIGGER ignore_eval`)
	mustNil(t, err)
	_, err = s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	mustNil(t, err)
	for _, query := range []string{`UPDATE swarm_eval_bindings SET model='other'`, `DELETE FROM swarm_eval_bindings`, `INSERT OR REPLACE INTO swarm_eval_bindings SELECT * FROM swarm_eval_bindings`} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatalf("history changed: %s", query)
		}
	}
}
