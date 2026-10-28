package server

import (
	"context"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// BUG: A rejected nonempty namespace delete revokes the owner's child-cleanup API (#71).
// SPEC: The native refusal is an atomic no-op; retain active/UID/owner and let
// the owner remove their children, then repeat the normal fenced deletion.
func TestNonemptySwarmDeleteLeavesOwnerAbleToCleanChildren(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	_, alice, _, id := actorsSetup(t, ts.testServer)
	sw, err := ts.srv.store.GetSwarm(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.templates[sw.AtespaceName+"/proof"] = &ateapi.ActorTemplate{Metadata: fakeMeta("proof", sw.AtespaceName)}
	fake.mu.Unlock()
	path := "/api/swarms/" + id
	r, b, _ := ts.do(alice, "DELETE", path, ts.csrf(alice), nil)
	expectStatus(t, "nonempty refusal", r, b, 409)
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "known swarm retained", r, b, 200)
	if b["state"] != "active" {
		t.Fatalf("atomic refusal poisoned child cleanup: %v", b)
	}
	saved, err := ts.srv.store.GetSwarm(context.Background(), id)
	if err != nil || saved.AtespaceUID == nil || *saved.AtespaceUID != *sw.AtespaceUID || saved.OwnerID != sw.OwnerID || saved.Retry != "" {
		t.Fatalf("refusal changed identity or retry: %+v, %v", saved, err)
	}
	r, b, _ = ts.do(alice, "GET", path+"/templates", "", nil)
	expectStatus(t, "owner retains workload access", r, b, 200)
	r, b, _ = ts.do(alice, "DELETE", path+"/templates/proof", ts.csrf(alice), nil)
	expectStatus(t, "owner cleans own template", r, b, 204)
	r, b, _ = ts.do(alice, "DELETE", path, ts.csrf(alice), nil)
	expectStatus(t, "fenced retry succeeds", r, b, 204)
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "swarm removed", r, b, 404)
}
