package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/store"
)

// TestUI290PolicyBrowser serves the normal embedded panel and real policy API
// with synthetic providers. An external browser answers both policy requests.
func TestUI290PolicyBrowser(t *testing.T) {
	descriptor := os.Getenv("BROOD_UI290_DESCRIPTOR")
	if descriptor == "" {
		t.Skip("explicit isolated browser acceptance only")
	}
	ctx := context.Background()
	authorize := func(context.Context) bool { return true }
	fixtures := map[string]*resultCollectionFixture{
		"exit": newResultCollectionFixtureWithGraph(t, false, policyExitGraph),
		"terminal": newResultCollectionFixtureWithGraph(t, false, strings.Replace(
			policyExitGraph, "{from: build, to: done}", "{from: build, to: done, condition: outcome=fail}", 1)),
	}
	addresses := make(map[string]map[string]string)
	for name, f := range fixtures {
		s := f.srv.store
		swarm, err := s.GetSwarm(ctx, f.swarm)
		if err != nil {
			t.Fatal(err)
		}
		bob, err := s.GetUserByUsername(ctx, "bob")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetSwarmMember(ctx, f.swarm, bob.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
		if name == "exit" {
			seedUI290History(t, f, authorize)
		}
		accepted := f.completion(t)
		if !accepted.OK || !f.acknowledge(t, accepted).OK {
			t.Fatal("synthetic completion was not acknowledged")
		}
		f.srv.steps.wait()
		steps, err := s.SwarmSteps(ctx, f.swarm)
		if err != nil || len(steps) != 1 || steps[0].Outcome != nil || f.calls.Load() != 1 {
			t.Fatalf("%s: source advanced before the browser answered: steps=%+v, error=%v", name, steps, err)
		}
		candidate, err := s.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
		if err != nil || candidate.Preview.Transition.Terminal != (name == "terminal") {
			t.Fatalf("%s: wrong retained candidate: %+v, error=%v", name, candidate, err)
		}
		addresses[name] = map[string]string{
			"origin": f.http.URL, "swarm": f.swarm, "project": swarm.ProjectID, "request": "policy:" + candidate.ID,
		}
	}

	done := make(chan struct{})
	var once sync.Once
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/done" {
			http.NotFound(w, r)
			return
		}
		once.Do(func() { close(done) })
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(control.Close)
	data, err := json.Marshal(map[string]any{"fixtures": addresses, "done": control.URL + "/done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(descriptor, data, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		t.Fatal("policy browser acceptance timed out")
	}
	for name, f := range fixtures {
		f.srv.steps.wait()
		swarm, err := f.srv.store.GetSwarm(ctx, f.swarm)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := f.srv.store.SwarmSteps(ctx, f.swarm)
		wantSteps, wantState := 2, "succeeded"
		if name == "terminal" {
			wantSteps, wantState = 1, "failed"
		}
		if err != nil || len(steps) != wantSteps || swarm.Execution != wantState ||
			steps[0].Outcome == nil || *steps[0].Outcome != "success" || f.calls.Load() != 1 {
			t.Fatalf("%s: wrong post-answer transition: execution=%s, steps=%+v, error=%v", name, swarm.Execution, steps, err)
		}
	}
}

func seedUI290History(t *testing.T, f *resultCollectionFixture, authorize func(context.Context) bool) {
	t.Helper()
	ctx := context.Background()
	s := f.srv.store
	alice, err := s.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{"Cancelled history first", "Cancelled history second", "Answered actor history"} {
		request, err := s.OpenActorRequest(ctx, store.ActorRequestInput{
			SwarmID: f.swarm, ActorUID: f.worker.UID, ActorIncarnation: f.worker.Incarnation,
			Kind: "text", Question: question,
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(question, "Cancelled") {
			if err := s.CancelSwarmRequest(ctx, f.swarm, request.ID, alice.ID, authorize); err != nil {
				t.Fatal(err)
			}
			continue
		}
		r, body, _ := f.do(f.alice, http.MethodPost, "/api/swarms/"+f.swarm+"/requests/"+request.ID+"/answer",
			f.csrf(f.alice), map[string]any{"response": "Synthetic answer evidence"})
		expectStatus(t, "seed answered actor history", r, body, http.StatusOK)
	}
}
