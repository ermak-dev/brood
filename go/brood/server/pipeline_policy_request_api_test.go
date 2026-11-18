package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"brood.dev/go/brood/store"
)

func TestPolicyRequestAPIExposesAndRequiresExactRevision(t *testing.T) {
	f := newResultCollectionFixture(t)
	response := f.completion(t)
	if !response.OK || !f.acknowledge(t, response).OK {
		t.Fatal("result completion was not acknowledged")
	}
	f.srv.steps.wait()
	candidate, err := f.srv.store.RecordPipelinePolicyCandidate(context.Background(), f.swarm, 1, "result", store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	id := "policy:" + candidate.ID
	r, body, _ := f.do(f.alice, "GET", "/api/swarms/"+f.swarm+"/requests", "", nil)
	expectStatus(t, "policy list", r, body, http.StatusOK)
	var list struct {
		Results []struct {
			ID, Origin string
			Revision   int64
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(encoded, &list) != nil || len(list.Results) != 1 || list.Results[0].ID != id || list.Results[0].Origin != "policy" || list.Results[0].Revision != 1 {
		t.Fatalf("policy revision not exposed: %s", body)
	}
	url := "/api/swarms/" + f.swarm + "/requests/" + id + "/answer"
	for _, input := range []map[string]any{{"response": "yes"}, {"response": "yes", "revision": 2}} {
		r, body, _ = f.do(f.alice, "POST", url, f.csrf(f.alice), input)
		expectStatus(t, "stale/omitted policy revision", r, body, http.StatusConflict)
	}
	f.srv.steps.wait()
	pending, err := f.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil || len(pending) != 1 || pending[0].Outcome != nil {
		t.Fatal("stale policy confirmation advanced the source")
	}
	for range 2 {
		r, body, _ = f.do(f.alice, "POST", url, f.csrf(f.alice), map[string]any{"response": "yes", "revision": 1})
		expectStatus(t, "policy confirmation", r, body, http.StatusOK)
		f.srv.steps.wait()
	}
	var answer struct {
		Origin, State, Delivery string
		Revision                int64
	}
	encoded, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(encoded, &answer) != nil || answer.Origin != "policy" || answer.State != "answered" || answer.Revision != 2 || answer.Delivery != "" {
		t.Fatalf("policy confirmation response: %s", body)
	}
	r, body, _ = f.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/requests/"+id+"/deliver", f.csrf(f.alice), nil)
	expectStatus(t, "policy is not actor delivery", r, body, http.StatusConflict)
	f.srv.steps.wait()
	steps, err := f.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil || len(steps) != 2 || steps[0].State != "completed" || steps[0].Outcome == nil || *steps[0].Outcome != "success" || steps[0].Report != "Candidate is committed; collect exact result evidence." || steps[1].NodeID != "review" || steps[1].State != "inflight" || steps[1].ActorUID != f.master.UID {
		t.Fatalf("exact confirmation did not release the verified successor with the original report: %+v %v", steps, err)
	}
	_, messages := f.dispatchServer.calls()
	if len(messages) != 2 || f.calls.Load() != 1 {
		t.Fatal("policy confirmation repeated source effects")
	}
}
