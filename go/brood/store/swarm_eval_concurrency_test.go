package store

// FEATURE: Eval retries serialize without another pass or decision (#293).
// SPEC: Concurrent exact binding/replay requests retain one immutable identity;
// terminal completion/stop cannot be restarted, and current ACL/source is checked.
// IMPLEMENTATION: FULL-сериализация, актуальные local reads и pinned identity.
// RESULT: IMPLEMENTED — normal/race и полный Store suite зелёные.
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestSwarmEvalConcurrentExactBindingAndReplay(t *testing.T) {
	ctx := context.Background()
	s, in := evalFixture(t)
	const n = 6
	var wg sync.WaitGroup
	bindings := make(chan SwarmEvalBinding, n)
	failures := make(chan error, n)
	for range n {
		wg.Go(func() { b, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize); bindings <- b; failures <- err })
	}
	wg.Wait()
	close(bindings)
	close(failures)
	for err := range failures {
		mustNil(t, err)
	}
	var first *SwarmEvalBinding
	for b := range bindings {
		if first == nil {
			first = &b
		} else if !reflect.DeepEqual(*first, b) {
			t.Fatal("concurrent binding changed identity")
		}
	}
	_, err := s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
	mustNil(t, err)
	requests, err := s.SwarmRequests(ctx, "two")
	mustNil(t, err)
	replay := SwarmEvalReplayInput{CandidateSwarmID: "two", RequestID: requests[0].ID, LauncherID: "u"}
	decisions := make(chan SwarmEvalReplay, n)
	failures = make(chan error, n)
	for range n {
		wg.Go(func() {
			d, err := s.ApplySwarmEvalReplay(ctx, replay, evalAuthorize, nil)
			decisions <- d
			failures <- err
		})
	}
	wg.Wait()
	close(decisions)
	close(failures)
	for err := range failures {
		mustNil(t, err)
	}
	var decision *SwarmEvalReplay
	for d := range decisions {
		if decision == nil {
			decision = &d
		} else if !reflect.DeepEqual(*decision, d) {
			t.Fatal("concurrent replay changed identity")
		}
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM swarm_eval_replays`).Scan(&count))
	if count != 1 {
		t.Fatalf("replay count: %d", count)
	}
	_, err = s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
	if !errors.Is(err, ErrSwarmEvalConflict) {
		t.Fatalf("terminal eval started another pass: %v", err)
	}
	_, err = s.StartSwarm(ctx, SwarmStart{SwarmID: "two", UserID: "u", Definition: "approve", Goal: startGoal, Commit: startCommit}, allow)
	if !errors.Is(err, ErrSwarmEvalConflict) {
		t.Fatalf("terminal eval escaped via ordinary start: %v", err)
	}
}

func TestSwarmEvalCurrentSourceIsRequiredOnExactBindingRetryAndStart(t *testing.T) {
	for _, project := range []string{"one", "two"} {
		t.Run(project, func(t *testing.T) {
			ctx := context.Background()
			s, in := evalFixture(t)
			_, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
			mustNil(t, err)
			mustNil(t, s.SetProjectRepository(ctx, project, "https://git.example.org/changed/repo.git"))
			_, err = s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
			if !errors.Is(err, ErrSwarmEvalConflict) {
				t.Fatalf("retry ignored changed source: %v", err)
			}
			_, err = s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
			if !errors.Is(err, ErrSwarmEvalConflict) {
				t.Fatalf("start ignored changed source: %v", err)
			}
		})
	}
}

func TestSwarmEvalBindingRejectsRecreatedCandidateAndNamespaceReplacement(t *testing.T) {
	for _, replace := range []string{"recreated", "namespace"} {
		t.Run(replace, func(t *testing.T) {
			ctx := context.Background()
			s, in := evalFixture(t)
			_, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
			mustNil(t, err)
			if replace == "namespace" {
				mustNil(t, s.SetSwarmAtespaceUID(ctx, "two", ptr("replacement-native-two")))
			} else {
				_, err = s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
				mustNil(t, err)
				mustNil(t, s.DeleteProject(ctx, "two"))
				seedProject(t, s, "two", "two")
				mustNil(t, s.AddMember(ctx, "two", "u"))
				mustNil(t, s.SetProjectRepository(ctx, "two", startOrigin))
				mustNil(t, s.CreateSwarm(ctx, Swarm{ID: "two", ProjectID: "two", OwnerID: "u", Name: "two", AtespaceName: "two", AtespaceUID: ptr("native-two"), State: "active"}))
			}
			_, err = s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
			if !errors.Is(err, ErrSwarmEvalConflict) {
				t.Fatalf("reused candidate identity: %v", err)
			}
		})
	}
}

func TestSwarmEvalBindingMarkerSurvivesCandidateProjectDeletion(t *testing.T) {
	ctx := context.Background()
	s, in := evalFixture(t)
	original, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
	mustNil(t, err)
	mustNil(t, s.DeleteProject(ctx, "two"))
	got, err := s.GetSwarmEvalBinding(ctx, "two")
	mustNil(t, err)
	if !reflect.DeepEqual(original, got) || !got.NonProduction {
		t.Fatal("deletion erased nonproduction marker")
	}
	_, err = s.StartSwarmEval(ctx, "two", "u", evalAuthorize)
	if err == nil {
		t.Fatal("retained history authorized a deleted candidate")
	}
}
