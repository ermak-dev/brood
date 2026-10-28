package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Build publication retains the same shard placement boundary (#49).
// SPEC: Golden-disabled push needs no actor slot, but cannot bypass selector
// enforcement; server-defaulted placement remains stable on an identical retry.
func TestPushTemplateEnforcesShardPlacementWithoutActorSlots(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(200)
	}))
	defer registry.Close()
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake, func(c *Config) { c.RegistryURL = registry.URL; c.RegistryImageHost = "localhost:5002" })
	_, alice, _, id := actorsSetup(t, ts.testServer)
	sw, err := ts.srv.store.GetSwarm(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), sw.ProjectID, store.ProjectCapacity{MaxActors: &zero, WorkerSelector: map[string]string{"shard": "one"}}); err != nil {
		t.Fatal(err)
	}
	image := "localhost:5002/" + sw.AtespaceName + "/dev@" + pushDigest("manifest")
	template := map[string]any{"metadata": map[string]string{"name": "pushed"}, "containers": []any{map[string]string{"name": "main", "image": image}}, "workerSelector": map[string]any{"matchLabels": map[string]string{"shard": "two"}}}
	request := map[string]any{"image": image, "actor_template": template, "egress_policy": map[string]any{}}
	path := "/api/swarms/" + id + "/builds/dev/templates"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), request)
	expectStatus(t, "foreign push selector", r, b, 400)
	fake.mu.Lock()
	count := len(fake.templates)
	fake.mu.Unlock()
	if count != 0 {
		t.Fatal("invalid push reached native template creation")
	}
	delete(template, "workerSelector")
	for range 2 {
		r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), request)
		expectStatus(t, "slotless idempotent push", r, b, 200)
	}
	fake.mu.Lock()
	saved := fake.templates[sw.AtespaceName+"/pushed"]
	actors := len(fake.actors)
	fake.mu.Unlock()
	if saved == nil || saved.GetWorkerSelector().GetMatchLabels()["shard"] != "one" || actors != 0 {
		t.Fatalf("push escaped placement or allocated an actor: template=%v, actors=%d", saved, actors)
	}
}
