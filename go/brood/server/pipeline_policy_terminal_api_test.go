package server

import (
	"context"
	"net/http"
	"testing"
)

func TestTerminalPolicyEvidenceAPIDistinguishesNoSuccessorFromMissingTarget(t *testing.T) {
	f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
	acceptTerminalCompletion(t, f, "fail")
	ctx := context.Background()
	candidate, err := f.srv.store.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
	if err != nil {
		t.Fatal(err)
	}
	url := "/api/swarms/" + f.swarm + "/requests/policy:" + candidate.ID + "/policy"
	for _, answer := range []bool{false, true} {
		if answer {
			r, body, _ := f.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/requests/policy:"+candidate.ID+"/answer", f.csrf(f.alice), map[string]any{"response": "yes", "revision": 1})
			expectStatus(t, "terminal confirmation", r, body, http.StatusOK)
			f.srv.steps.wait()
		}
		r, body, _ := f.do(f.alice, "GET", url, "", nil)
		expectStatus(t, "retained terminal evidence", r, body, http.StatusOK)
		evidence, ok := body["evidence"].(map[string]any)
		if !ok || body["terminal"] != true || body["target"] != "" || body["targetKind"] != "exit" || body["initial"] != false || body["boundary"] != "result" || evidence["known"] != true || evidence["artifact"] != candidate.Preview.Input.Evidence.Artifact {
			t.Fatalf("evidence API disguises no-edge failure as a missing target: %+v", body)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatal("reading retained terminal evidence repeated a guest observation")
	}
}
