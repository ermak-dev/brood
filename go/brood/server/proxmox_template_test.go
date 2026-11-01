package server

// FEATURE: Read-only managed profile templates in bound native swarms.
// SPEC: GET projects virtual templates without native writes; installed profiles
// cannot be overwritten, deleted, or shadowed by either native create path.

import (
	"context"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func TestProxmoxGeneratedTemplateAndNativeCollision(t *testing.T) {
	cfg, err := loadProxmox(runtimeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake, func(c *Config) { c.proxmox = cfg.proxmox })
	admin, alice, _, id := actorsSetup(t, ts.testServer)
	name := "claude-proxmox-brood-proxmox"
	path := "/api/swarms/" + id + "/templates"
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-pools/brood-proxmox/activate", ts.csrf(admin), nil)
	expectStatus(t, "activation", r, b, 201)
	var uid any
	for range 2 {
		r, b, _ = ts.do(alice, "GET", path, "", nil)
		expectStatus(t, "merged templates", r, b, 200)
		results := b["results"].([]any)
		if len(results) != 1 {
			t.Fatalf("generated template missing: %#v", b)
		}
		item := results[0].(map[string]any)
		meta := item["metadata"].(map[string]any)
		if meta["name"] != name || item["provider"] != "proxmox" || item["readOnly"] != true {
			t.Fatalf("wrong managed template: %#v", item)
		}
		if uid != nil && uid != meta["uid"] {
			t.Fatal("GET changed virtual identity")
		}
		uid = meta["uid"]
	}
	fake.mu.Lock()
	nativeCount := len(fake.templates)
	fake.mu.Unlock()
	if nativeCount != 0 {
		t.Fatal("GET wrote native template")
	}
	r, b, _ = ts.do(alice, "GET", path+"/"+name, "", nil)
	expectStatus(t, "get template", r, b, 200)
	r, b, _ = ts.do(alice, "DELETE", path+"/"+name, ts.csrf(alice), nil)
	expectStatus(t, "readonly delete", r, b, 409)
	body := map[string]any{"metadata": map[string]string{"name": name}, "resources": map[string]any{"limits": map[string]string{"cpuMillicores": "1000", "memoryBytes": "1073741824"}}}
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), body)
	expectStatus(t, "native shadow", r, b, 409)
	sw, err := ts.srv.store.GetSwarm(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.templates[sw.AtespaceName+"/"+name] = &ateapi.ActorTemplate{Metadata: fakeMeta(name, sw.AtespaceName)}
	fake.mu.Unlock()
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "late foreign collision", r, b, 409)
}
