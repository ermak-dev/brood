package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"brood.dev/go/brood/store"
)

func TestPolicyDetailAPIExposesRetainedEvidenceToSwarmReaders(t *testing.T) {
	f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
	accepted := f.completion(t)
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatal("completion receipt rejected")
	}
	f.srv.steps.wait()
	ctx := context.Background()
	candidate, err := f.srv.store.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
	if err != nil {
		t.Fatal(err)
	}
	url := "/api/swarms/" + f.swarm + "/requests/policy:" + candidate.ID + "/policy"
	r, body, _ := f.do(f.alice, "GET", url, "", nil)
	expectStatus(t, "owner candidate detail", r, body, http.StatusOK)
	want := map[string]any{
		"id": candidate.ID, "version": float64(1), "swarmId": f.swarm,
		"sequence": float64(1), "pass": float64(1), "policyRevision": float64(1),
		"boundary": "result", "createdAt": candidate.CreatedAt, "target": "done", "targetKind": "exit",
		"initial": false, "actionsKnown": false, "actions": []any{},
		"rules":    []any{map[string]any{"id": "storage", "paths": []any{"go/**"}, "require": "human"}},
		"evidence": map[string]any{"id": candidate.Preview.EvidenceID, "known": true, "complete": true, "artifact": candidate.Preview.Input.Evidence.Artifact, "producerUid": f.parent.UID, "paths": []any{"go/result.go"}},
		"decision": map[string]any{"state": "waiting_human", "matches": []any{map[string]any{"ruleId": "storage", "paths": []any{"go/result.go"}}}, "human": []any{"storage"}, "review": []any{}, "denied": []any{}, "unknown": []any{}, "notApplicable": []any{}},
	}
	raw, _ := json.Marshal(body)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate detail lost retained evidence:\ngot %s\nwant %#v", raw, want)
	}
	before := string(raw)
	viewer, err := f.srv.store.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetSwarmMember(ctx, f.swarm, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	client := newClient(t)
	f.login(client, "bob", "bob-password-1")
	r, body, _ = f.do(client, "GET", url, "", nil)
	expectStatus(t, "viewer candidate detail", r, body, http.StatusOK)
	raw, _ = json.Marshal(body)
	if string(raw) != before {
		t.Fatal("reader changed retained candidate")
	}
	f.stop()
	r, body, _ = f.do(f.alice, "GET", url, "", nil)
	expectStatus(t, "stopped candidate history", r, body, http.StatusOK)
	raw, _ = json.Marshal(body)
	if string(raw) != before {
		t.Fatal("stop reinterpreted historical decision")
	}
	if f.calls.Load() != 1 {
		t.Fatal("detail read retried guest result observation")
	}
}

func TestPolicyDetailAPIDoesNotRevealOtherScopesOrAcceptWrites(t *testing.T) {
	f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
	accepted := f.completion(t)
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatal("completion receipt rejected")
	}
	f.srv.steps.wait()
	ctx := context.Background()
	candidate, err := f.srv.store.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
	if err != nil {
		t.Fatal(err)
	}
	url := "/api/swarms/" + f.swarm + "/requests/policy:" + candidate.ID + "/policy"
	outsider := newClient(t)
	f.login(outsider, "bob", "bob-password-1")
	r, body, _ := f.do(outsider, "GET", url, "", nil)
	expectStatus(t, "ungranted candidate", r, body, http.StatusNotFound)
	anonymous := newClient(t)
	r, body, _ = f.do(anonymous, "GET", url, "", nil)
	expectStatus(t, "anonymous candidate", r, body, http.StatusUnauthorized)
	other, err := f.srv.store.GetSwarm(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	other.ID, other.Name, other.AtespaceName, other.AtespaceUID = store.NewID(), "other-policy-scope", "other-policy-scope", nil
	if err := f.srv.store.CreateSwarm(ctx, other); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/swarms/" + other.ID + "/requests/policy:" + candidate.ID + "/policy",
		"/api/swarms/foreign/requests/policy:" + candidate.ID + "/policy",
		"/api/swarms/" + f.swarm + "/requests/policy:missing/policy",
		"/api/swarms/" + f.swarm + "/requests/" + candidate.ID + "/policy",
	} {
		r, body, _ = f.do(f.alice, "GET", path, "", nil)
		expectStatus(t, "scope and request origin", r, body, http.StatusNotFound)
	}
	r, body, _ = f.do(f.alice, "POST", url, f.csrf(f.alice), map[string]any{"decision": "allowed"})
	expectStatus(t, "immutable detail", r, body, http.StatusMethodNotAllowed)
	if _, err := f.srv.store.GetPipelinePolicyConfirmation(ctx, f.swarm, candidate.ID); err != store.ErrNotFound {
		t.Fatalf("detail created confirmation: %v", err)
	}
	steps, err := f.srv.store.SwarmSteps(ctx, f.swarm)
	if err != nil || len(steps) != 1 || steps[0].Outcome != nil {
		t.Fatal("detail changed graph")
	}
}
