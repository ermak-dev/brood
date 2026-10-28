package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// FEATURE: Configure and enforce shard limits through authorized HTTP operations (#49).
// SPEC: Nullable decimal limits preserve zero; actors, updates/resume and pending
// golden initialization cannot escape resource limits or required worker labels.
func quotaHTTPFixture(t *testing.T) (*substrateServer, *fakeAteapi, *http.Client, *http.Client, string, string) {
	t.Helper()
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, alice, _, id := actorsSetup(t, ts.testServer)
	sw, err := ts.srv.store.GetSwarm(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.templates[sw.AtespaceName+"/base"] = inventoryTemplate(sw.AtespaceName, "base", "base-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
	fake.mu.Unlock()
	return ts, fake, admin, alice, id, sw.ProjectID
}

func TestProjectCapacityRejectsNullWithoutClearingLimits(t *testing.T) {
	ts, _, admin, _, _, project := quotaHTTPFixture(t)
	one := int64(1)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), project, store.ProjectCapacity{MaxActors: &one}); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest("PUT", ts.http.URL+"/api/projects/"+project+"/capacity", strings.NewReader("null"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", ts.http.URL)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRFToken", ts.csrf(admin))
	response, err := admin.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("null configuration returned %d; want400", response.StatusCode)
	}
	saved, err := ts.srv.store.GetProjectCapacity(context.Background(), project)
	if err != nil || saved.MaxActors == nil || *saved.MaxActors != 1 {
		t.Fatalf("null wiped previous settings: %+v, %v", saved, err)
	}
}

func TestProjectCapacityHTTPSettingsAndUsage(t *testing.T) {
	ts, _, admin, alice, swarm, project := quotaHTTPFixture(t)
	path := "/api/projects/" + project
	r, b, _ := ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "member shard detail", r, b, 200)
	capacity, ok := b["capacity"].(map[string]any)
	if !ok || capacity["max_actors"] != nil {
		t.Fatalf("unlimited capacity missing: %v", b)
	}
	usage, ok := b["usage"].(map[string]any)
	if !ok || usage["swarms"] != "1" || usage["actors"] != "0" {
		t.Fatalf("authoritative usage missing: %v", b)
	}
	settings := map[string]any{"max_actors": "0", "max_swarms": nil, "max_cpu_millicores": "2000", "max_memory_bytes": "9223372036854775807", "worker_selector": map[string]string{"shard": "one"}}
	r, b, _ = ts.do(alice, "PUT", path+"/capacity", ts.csrf(alice), settings)
	expectStatus(t, "member cannot configure", r, b, 403)
	r, b, _ = ts.do(admin, "PUT", path+"/capacity", "", settings)
	expectStatus(t, "capacity csrf", r, b, 403)
	r, b, _ = ts.do(admin, "PUT", path+"/capacity", ts.csrf(admin), settings)
	expectStatus(t, "configure zero capacity", r, b, 204)
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "read configured capacity", r, b, 200)
	capacity = b["capacity"].(map[string]any)
	if capacity["max_actors"] != "0" || capacity["max_memory_bytes"] != "9223372036854775807" {
		t.Fatalf("limits lost precision or zero: %v", capacity)
	}
	for _, invalid := range []map[string]any{{"max_actors": "-1"}, {"max_actors": "9223372036854775808"}, {"max_actors": float64(1)}, {"max_actors": "1e3"}, {"unexpected": true}} {
		r, b, _ = ts.do(admin, "PUT", path+"/capacity", ts.csrf(admin), invalid)
		expectStatus(t, "invalid limits", r, b, 400)
	}
	r, b, _ = ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "denied"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "zero actor limit", r, b, 409)
}

func TestActorHTTPQuotaAndShardSelector(t *testing.T) {
	ts, fake, _, alice, swarm, project := quotaHTTPFixture(t)
	one := int64(1)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), project, store.ProjectCapacity{MaxActors: &one, WorkerSelector: map[string]string{"shard": "one"}}); err != nil {
		t.Fatal(err)
	}
	base := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(alice, "POST", base, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "wrong"}, "actorTemplate": map[string]string{"name": "base"}, "workerSelector": map[string]any{"matchLabels": map[string]string{"shard": "two"}}})
	expectStatus(t, "foreign selector", r, b, 400)
	r, b, _ = ts.do(alice, "POST", base, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "first"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "first actor", r, b, 201)
	selector := b["workerSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if selector["shard"] != "one" {
		t.Fatalf("shard selector not defaulted: %v", b)
	}
	r, b, _ = ts.do(alice, "POST", base, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "second"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "actor limit exhausted", r, b, 409)
	fake.mu.Lock()
	count := len(fake.actors)
	fake.mu.Unlock()
	if count != 1 {
		t.Fatalf("refusal reached native create: actors=%d", count)
	}
	r, b, _ = ts.do(alice, "POST", base+"/first", ts.csrf(alice), map[string]string{"action": "resume"})
	expectStatus(t, "resume does not reserve twice", r, b, 200)
}

func TestTemplateDeletionCannotInvalidateLiveReservations(t *testing.T) {
	ts, _, _, alice, swarm, _ := quotaHTTPFixture(t)
	base := "/api/swarms/" + swarm
	r, b, _ := ts.do(alice, "POST", base+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "existing"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "create referenced actor", r, b, 201)
	r, b, _ = ts.do(alice, "DELETE", base+"/templates/base", ts.csrf(alice), nil)
	expectStatus(t, "keep referenced template immutable", r, b, 409)
	r, b, _ = ts.do(alice, "GET", base+"/templates/base", "", nil)
	expectStatus(t, "template retained", r, b, 200)
	r, b, _ = ts.do(alice, "DELETE", base+"/actors/existing", ts.csrf(alice), nil)
	expectStatus(t, "remove reference", r, b, 204)
	r, b, _ = ts.do(alice, "DELETE", base+"/templates/base", ts.csrf(alice), nil)
	expectStatus(t, "remove unreferenced template", r, b, 204)
}

func TestTemplateDeletionAllowsManagedGoldenCleanupAtExhaustedQuota(t *testing.T) {
	ts, fake, _, alice, swarm, project := quotaHTTPFixture(t)
	sw, err := ts.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil {
		t.Fatal(err)
	}
	template := inventoryTemplate(sw.AtespaceName, "pending", "pending-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED)
	fake.mu.Lock()
	fake.templates[sw.AtespaceName+"/pending"] = template
	fake.actors[sw.AtespaceName+"/pending-uid"] = &ateapi.Actor{Metadata: fakeMeta("pending-uid", sw.AtespaceName), ActorTemplate: &ateapi.ObjectRef{Atespace: sw.AtespaceName, Name: "pending"}}
	fake.mu.Unlock()
	zero := int64(0)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), project, store.ProjectCapacity{MaxActors: &zero}); err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(alice, "DELETE", "/api/swarms/"+swarm+"/templates/pending", ts.csrf(alice), nil)
	expectStatus(t, "native workflow may clean its managed golden", r, b, 204)
	fake.mu.Lock()
	_, exists := fake.templates[sw.AtespaceName+"/pending"]
	fake.mu.Unlock()
	if exists {
		t.Fatal("managed template deletion did not reach native workflow")
	}
}

func TestActorHTTPTemplateChangeCannotGrowPastResourceLimit(t *testing.T) {
	ts, fake, _, alice, swarm, project := quotaHTTPFixture(t)
	path := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "existing"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "create reservation", r, b, 201)
	sw, err := ts.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil {
		t.Fatal(err)
	}
	large := inventoryTemplate(sw.AtespaceName, "large", "large-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
	large.Resources.Limits.CpuMillicores = 2000
	fake.mu.Lock()
	fake.templates[sw.AtespaceName+"/large"] = large
	fake.mu.Unlock()
	cpu := int64(1000)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), project, store.ProjectCapacity{MaxCPUMillicores: &cpu, WorkerSelector: map[string]string{"shard": "one"}}); err != nil {
		t.Fatal(err)
	}
	r, b, _ = ts.do(alice, "PUT", path+"/existing", ts.csrf(alice), map[string]any{"metadata": map[string]any{"uid": "uid-existing", "version": "7"}, "actorTemplate": map[string]string{"name": "large"}})
	expectStatus(t, "resource growth refused", r, b, 409)
	fake.mu.Lock()
	ref := fake.actors[sw.AtespaceName+"/existing"].GetActorTemplate().GetName()
	fake.mu.Unlock()
	if ref != "base" {
		t.Fatal("quota refusal changed native template reference")
	}
	r, b, _ = ts.do(alice, "POST", path+"/existing", ts.csrf(alice), map[string]string{"action": "resume"})
	expectStatus(t, "resume existing reservation", r, b, 200)
	if b["workerSelector"].(map[string]any)["matchLabels"].(map[string]any)["shard"] != "one" {
		t.Fatalf("resume did not install default shard selector: %v", b)
	}
}

func TestShardHTTPUsageIncludesOtherMembersWithoutExposingTheirSwarms(t *testing.T) {
	ts, fake, _, alice, swarm, project := quotaHTTPFixture(t)
	bob := newClient(t)
	ts.login(bob, "bob", "bob-password-1")
	r, b, _ := ts.do(bob, "POST", "/api/projects/"+project+"/swarms", ts.csrf(bob), map[string]string{"name": "private-bob"})
	expectStatus(t, "other member swarm", r, b, 201)
	otherID, space := b["id"].(string), b["atespace_name"].(string)
	fake.mu.Lock()
	fake.templates[space+"/private-template"] = inventoryTemplate(space, "private-template", "other-template-uid", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
	fake.actors[space+"/private-actor"] = &ateapi.Actor{Metadata: fakeMeta("private-actor", space), ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "private-template"}, Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED}}
	fake.mu.Unlock()
	one := int64(1)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), project, store.ProjectCapacity{MaxActors: &one}); err != nil {
		t.Fatal(err)
	}
	r, b, raw := ts.do(alice, "GET", "/api/projects/"+project, "", nil)
	expectStatus(t, "shared shard usage", r, b, 200)
	if b["usage"].(map[string]any)["actors"] != "1" || strings.Contains(raw, otherID) || strings.Contains(raw, "private-") {
		t.Fatalf("wrong or over-disclosing usage: %s", raw)
	}
	r, b, _ = ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "denied"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "other member consumes shard budget", r, b, 409)
	fake.mu.Lock()
	delete(fake.templates, space+"/private-template")
	fake.mu.Unlock()
	r, b, raw = ts.do(alice, "GET", "/api/projects/"+project, "", nil)
	expectStatus(t, "unavailable usage is explicit", r, b, 200)
	if b["usage"] != nil || b["usage_available"] != false || strings.Contains(raw, space) || strings.Contains(raw, "private-template") {
		t.Fatalf("unknown resources became zero or disclosed private identifiers: %s", raw)
	}
	r, b, raw = ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "unavailable"}, "actorTemplate": map[string]string{"name": "base"}})
	expectStatus(t, "unknown inventory fails closed", r, b, 503)
	if strings.Contains(raw, space) || strings.Contains(raw, "private-template") {
		t.Fatalf("quota failure disclosed private identifiers: %s", raw)
	}
}

func TestPendingGoldenReservationExistsBeforeNativeActorBirth(t *testing.T) {
	ts, fake, _, alice, swarm, project := quotaHTTPFixture(t)
	one := int64(1)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), project, store.ProjectCapacity{MaxActors: &one}); err != nil {
		t.Fatal(err)
	}
	body := func(name string) map[string]any {
		return map[string]any{"metadata": map[string]string{"name": name}, "containers": []any{map[string]string{"name": "main", "image": "busybox"}}, "resources": map[string]any{"limits": map[string]string{"cpuMillicores": "1000", "memoryBytes": "1024"}}}
	}
	path := "/api/swarms/" + swarm + "/templates"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), body("first-golden"))
	expectStatus(t, "first pending golden", r, b, 201)
	fake.mu.Lock()
	actors := len(fake.actors)
	fake.mu.Unlock()
	if actors != 0 {
		t.Fatal("fixture must exercise the asynchronous gap before actor creation")
	}
	r, b, _ = ts.do(alice, "GET", "/api/projects/"+project, "", nil)
	expectStatus(t, "pending usage", r, b, 200)
	if b["usage"].(map[string]any)["actors"] != "1" {
		t.Fatalf("pending golden did not reserve capacity: %v", b)
	}
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), body("second-golden"))
	expectStatus(t, "pending golden fills slot", r, b, 409)
}

func TestTemplateHTTPPendingGoldenConsumesQuota(t *testing.T) {
	ts, fake, _, alice, swarm, project := quotaHTTPFixture(t)
	zero := int64(0)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), project, store.ProjectCapacity{MaxActors: &zero}); err != nil {
		t.Fatal(err)
	}
	body := func(name string, disabled bool) map[string]any {
		result := map[string]any{"metadata": map[string]string{"name": name}, "containers": []any{map[string]any{"name": "main", "image": "busybox"}}, "resources": map[string]any{"limits": map[string]string{"cpuMillicores": "1000", "memoryBytes": "1024"}}}
		if disabled {
			result["snapshotConfig"] = map[string]any{"goldenSnapshotConfig": map[string]string{"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"}}
		}
		return result
	}
	path := "/api/swarms/" + swarm + "/templates"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), body("pending", false))
	expectStatus(t, "implicit golden must reserve actor", r, b, 409)
	spoofed := body("spoofed", false)
	spoofed["status"] = map[string]any{"goldenSnapshotStatus": map[string]any{"goldenTag": map[string]string{"atespace": "foreign", "name": "pretend-ready"}}}
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), spoofed)
	expectStatus(t, "client status cannot bypass golden reservation", r, b, 409)
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), body("disabled", true))
	expectStatus(t, "disabled golden needs no actor slot", r, b, 201)
	fake.mu.Lock()
	count := len(fake.templates)
	fake.mu.Unlock()
	if count != 2 {
		t.Fatalf("golden quota refusal reached native create: templates=%d", count)
	}
}
