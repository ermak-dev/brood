package server

import (
	"context"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Native admission retains live authorization after its final namespace read (#49).
// SPEC: Revoking the user's activity while an upstream read is in flight must
// prevent the subsequent actor/template write, even with a valid cached request.
func TestGoldenPolicyAuthorizationPrecedesInventoryAndQuota(t *testing.T) {
	ts, fake, _, alice, swarm, project := quotaHTTPFixture(t)
	zero := int64(0)
	if err := ts.srv.store.SetProjectCapacity(context.Background(), project, store.ProjectCapacity{MaxActors: &zero}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	before := fake.namespaceReads
	fake.mu.Unlock()
	body := map[string]any{"metadata": map[string]string{"name": "denied-policy"}, "containers": []any{map[string]string{"name": "main", "image": "busybox"}}, "resources": map[string]any{"limits": map[string]string{"cpuMillicores": "1000", "memoryBytes": "1024"}}, "goldenEgressPolicy": map[string]any{}}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/templates", ts.csrf(alice), body)
	expectStatus(t, "policy permission before quota", r, b, 403)
	fake.mu.Lock()
	after := fake.namespaceReads
	fake.mu.Unlock()
	if after != before {
		t.Fatal("forbidden embedded policy reached native inventory")
	}
}

func TestOrdinaryTemplatePublicationCannotInstallGoldenPolicy(t *testing.T) {
	ts, fake, _, alice, swarm, _ := quotaHTTPFixture(t)
	body := map[string]any{"metadata": map[string]string{"name": "denied-policy"}, "containers": []any{map[string]string{"name": "main", "image": "busybox"}}, "resources": map[string]any{"limits": map[string]string{"cpuMillicores": "1000", "memoryBytes": "1024"}}, "goldenEgressPolicy": map[string]any{}}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/templates", ts.csrf(alice), body)
	expectStatus(t, "embedded native policy requires administrator", r, b, 403)
	fake.mu.Lock()
	count := len(fake.templates)
	fake.mu.Unlock()
	if count != 1 {
		t.Fatal("non-admin embedded policy reached native publication")
	}
}

func TestNativeCapacityMutationRechecksRevocationAfterBindingRead(t *testing.T) {
	for _, resource := range []string{"actors", "templates"} {
		t.Run(resource, func(t *testing.T) {
			ts, fake, _, alice, swarm, _ := quotaHTTPFixture(t)
			user, err := ts.srv.store.GetUserByUsername(context.Background(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			fake.beforeNamespaceGet = func() {
				fake.beforeNamespaceGet = func() {
					if err := ts.srv.store.SetUserDisabled(context.Background(), user.ID, true); err != nil {
						t.Error(err)
					}
				}
			}
			fake.mu.Unlock()
			body := map[string]any{"metadata": map[string]string{"name": "denied"}, "actorTemplate": map[string]string{"name": "base"}}
			if resource == "templates" {
				body = map[string]any{"metadata": map[string]string{"name": "denied"}, "containers": []any{map[string]string{"name": "main", "image": "busybox"}}, "snapshotConfig": map[string]any{"goldenSnapshotConfig": map[string]string{"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"}}}
			}
			r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/"+resource, ts.csrf(alice), body)
			expectStatus(t, "revoked before native write", r, b, 403)
			fake.mu.Lock()
			actors, templates := len(fake.actors), len(fake.templates)
			fake.mu.Unlock()
			if actors != 0 || templates != 1 {
				t.Fatalf("revoked request mutated native resources: actors=%d, templates=%d", actors, templates)
			}
		})
	}
}
