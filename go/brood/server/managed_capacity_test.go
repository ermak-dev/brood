package server

// FEATURE: One shard budget and selector boundary across both providers.
// SPEC: Native create sees retained managed reservations; managed create sees
// native reservations, and neither provider bypasses shard selectors or limits.
// IMPLEMENTATION: Mixed inventory is charged once; reset validates the current
// shard selector and pool state while drain preserves current allocations.
// RESULT: IMPLEMENTED — both providers share admission and retained reservations.
// STATUS: DONE

import (
	"context"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func TestNativeAdmissionCountsManagedReservations(t *testing.T) {
	ts, alice, _, id, _, _ := managedSetup(t)
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "managed"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create managed", r, b, 201)
	sw, _ := ts.srv.store.GetSwarm(context.Background(), id)
	one := int64(1)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), sw.ProjectID, store.ProjectCapacity{MaxActors: &one}); err != nil {
		t.Fatal(err)
	}
	r, b, _ = ts.do(alice, "GET", "/api/projects/"+sw.ProjectID, "", nil)
	expectStatus(t, "aggregate usage", r, b, 200)
	if b["usage"].(map[string]any)["actors"] != "1" {
		t.Fatalf("managed reservation missing from shard usage: %#v", b)
	}
	ts.fake.mu.Lock()
	ts.fake.templates[sw.AtespaceName+"/native"] = inventoryTemplate(sw.AtespaceName, "native", "native-template", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
	ts.fake.mu.Unlock()
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "native"}, "actorTemplate": map[string]string{"name": "native"}})
	expectStatus(t, "native mixed quota", r, b, 409)
}

func TestManagedDrainRetainsCurrentActorAndBlocksNewPlacements(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	path := "/api/swarms/" + id + "/actors"
	body := map[string]any{"metadata": map[string]string{"name": "current"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}}
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), body)
	expectStatus(t, "create", r, b, 201)
	hub.connected = true
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	r, b, _ = ts.do(admin, "POST", "/api/proxmox-pools/brood-proxmox/drain", ts.csrf(admin), nil)
	expectStatus(t, "drain", r, b, 200)
	ts.srv.managed.reconcileOnce(context.Background())
	a := mustManagedActor(t, ts, id)
	if a.Phase != "running" || pve.stops != 0 || pve.deletes != 0 {
		t.Fatal("drain interrupted existing actor")
	}
	r, b, _ = ts.do(alice, "POST", path+"/current", ts.csrf(alice), map[string]string{"action": "reset"})
	expectStatus(t, "reset while draining", r, b, 409)
	if detail, _ := b["detail"].(string); !strings.Contains(detail, "pool") {
		t.Fatalf("drain refusal hides its reason: %q", detail)
	}
	if pve.stops != 0 || pve.deletes != 0 {
		t.Fatal("drained reset mutated the existing CT")
	}
	body["metadata"] = map[string]string{"name": "new-placement"}
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), body)
	expectStatus(t, "new placement while draining", r, b, 409)
	r, b, _ = ts.do(alice, "POST", path+"/current", ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "stop while draining", r, b, 200)
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	if a = mustManagedActor(t, ts, id); a.Phase != "stopped" {
		t.Fatal("drain blocked existing cleanup")
	}
	if pve.creates != 1 {
		t.Fatal("drain admitted another CT")
	}
}

func TestManagedResetEnforcesCurrentShardSelector(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "selector"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	hub.connected = true
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	sw, _ := ts.srv.store.GetSwarm(context.Background(), id)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), sw.ProjectID, store.ProjectCapacity{WorkerSelector: map[string]string{"ate.dev/worker-pool": "another"}}); err != nil {
		t.Fatal(err)
	}
	r, b, _ = ts.do(alice, "POST", path+"/selector", ts.csrf(alice), map[string]string{"action": "reset"})
	expectStatus(t, "reset outside current shard selector", r, b, 400)
	if pve.stops != 0 || pve.deletes != 0 || pve.creates != 1 {
		t.Fatal("rejected reset reached provider writes")
	}
}

func TestManagedAdmissionCountsNativeAndEnforcesSelector(t *testing.T) {
	ts, alice, _, id, pve, _ := managedSetup(t)
	sw, _ := ts.srv.store.GetSwarm(context.Background(), id)
	one := int64(1)
	ts.srv.store.SetProjectCapacity(context.Background(), sw.ProjectID, store.ProjectCapacity{MaxActors: &one})
	ts.fake.mu.Lock()
	ts.fake.templates[sw.AtespaceName+"/native"] = inventoryTemplate(sw.AtespaceName, "native", "native-template", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
	ts.fake.actors[sw.AtespaceName+"/native"] = &ateapi.Actor{Metadata: fakeMeta("native", sw.AtespaceName), ActorTemplate: &ateapi.ObjectRef{Name: "native", Atespace: sw.AtespaceName}}
	ts.fake.mu.Unlock()
	path := "/api/swarms/" + id + "/actors"
	body := map[string]any{"metadata": map[string]string{"name": "managed"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}}
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), body)
	expectStatus(t, "managed mixed quota", r, b, 409)
	ts.srv.store.SetProjectCapacity(context.Background(), sw.ProjectID, store.ProjectCapacity{WorkerSelector: map[string]string{"ate.dev/worker-pool": "another"}})
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), body)
	expectStatus(t, "required pool selector", r, b, 400)
	if pve.creates != 0 {
		t.Fatal("rejected placement reached PVE write")
	}
}
