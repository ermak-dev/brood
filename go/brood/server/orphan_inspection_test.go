package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// FEATURE: Inspect orphan provenance before adopting a local binding (#68).
// SPEC: Administrator-only, expected UID required, no native/local mutations;
// return names and image references, never container arguments or credentials.
func TestOrphanInspectionReadOnlyAndUIDFenced(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, alice, _, known := actorsSetup(t, ts.testServer)
	name := app.AtespaceName(recoveryTestID)
	addRecoveryNamespace(fake, name, "expected-uid")
	image := "registry.example/proof@sha256:" + strings.Repeat("a", 64)
	fake.mu.Lock()
	fake.templates[name+"/proof"] = &ateapi.ActorTemplate{Metadata: fakeMeta("proof", name), Containers: []*ateapi.Container{{Name: "main", Image: image, Args: []string{"credential-must-not-be-returned"}}}}
	fake.actors[name+"/job"] = &ateapi.Actor{Metadata: fakeMeta("job", name), ActorTemplate: &ateapi.ObjectRef{Atespace: name, Name: "proof"}}
	fake.mu.Unlock()
	path := "/api/admin/orphan-swarms/" + recoveryTestID + "?atespace_uid=expected-uid"
	before := recoveryReads(fake)
	for _, tc := range []struct {
		client *http.Client
		status int
	}{{alice, 403}, {newClient(t), 401}} {
		r, b, _ := ts.do(tc.client, "GET", path, "", nil)
		expectStatus(t, "unauthorized inspection", r, b, tc.status)
	}
	if recoveryReads(fake) != before {
		t.Fatal("unauthorized inspection reached native API")
	}
	r, b, raw := ts.do(admin, "GET", path, "", nil)
	expectStatus(t, "inspect", r, b, 200)
	if b["atespace_name"] != name || b["atespace_uid"] != "expected-uid" || !strings.Contains(raw, image) || strings.Contains(raw, "credential-must-not-be-returned") {
		t.Fatalf("unsafe or incomplete inspection: %s", raw)
	}
	if len(b["actors"].([]any)) != 1 || len(b["templates"].([]any)) != 1 {
		t.Fatalf("wrong provenance: %v", b)
	}
	if _, err := ts.srv.store.GetSwarm(context.Background(), recoveryTestID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("inspection adopted a binding: %v", err)
	}
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/admin/orphan-swarms/" + recoveryTestID, 400},
		{"/api/admin/orphan-swarms/" + recoveryTestID + "?atespace_uid=wrong", 409},
		{"/api/admin/orphan-swarms/" + known + "?atespace_uid=expected-uid", 409},
	} {
		r, b, _ := ts.do(admin, "GET", tc.path, "", nil)
		expectStatus(t, "invalid inspection", r, b, tc.status)
	}
	fake.mu.Lock()
	count, uid, version := len(fake.atespaces), fake.atespaces[name].Metadata.Uid, fake.atespaces[name].Metadata.Version
	fake.mu.Unlock()
	if count != 2 || uid != "expected-uid" || version != 7 {
		t.Fatal("inspection mutated native resources")
	}
}

func TestOrphanInspectionRechecksLocalAuthorityAndBindings(t *testing.T) {
	for _, scenario := range []string{"administrator demoted", "binding claimed"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newFakeAteapi()
			ts := newSubstrateServer(t, fake)
			admin, _, _, knownID := actorsSetup(t, ts.testServer)
			name := app.AtespaceName(recoveryTestID)
			addRecoveryNamespace(fake, name, "expected-uid")
			known, err := ts.srv.store.GetSwarm(context.Background(), knownID)
			if err != nil {
				t.Fatal(err)
			}
			root, err := ts.srv.store.GetUserByUsername(context.Background(), "root")
			if err != nil {
				t.Fatal(err)
			}
			if err := ts.srv.store.CreateUser(context.Background(), store.User{ID: "other-admin", Username: "other-admin", PasswordHash: "unused", IsAdmin: true}); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			fake.beforeNamespaceGet = func() {
				var err error
				if scenario == "administrator demoted" {
					err = ts.srv.store.SetUserAdmin(context.Background(), root.ID, false)
				} else {
					uid := "expected-uid"
					err = ts.srv.store.CreateSwarm(context.Background(), store.Swarm{ID: recoveryTestID, ProjectID: known.ProjectID, OwnerID: root.ID, Name: "claimed", AtespaceName: name, AtespaceUID: &uid, State: "active"})
				}
				if err != nil {
					t.Error(err)
				}
			}
			fake.mu.Unlock()
			r, b, _ := ts.do(admin, "GET", "/api/admin/orphan-swarms/"+recoveryTestID+"?atespace_uid=expected-uid", "", nil)
			want := 409
			if scenario == "administrator demoted" {
				want = 403
			}
			expectStatus(t, scenario, r, b, want)
		})
	}
}

func TestOrphanInspectionRechecksNativeIdentity(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, _, _, _ := actorsSetup(t, ts.testServer)
	name := app.AtespaceName(recoveryTestID)
	addRecoveryNamespace(fake, name, "expected-uid")
	fake.mu.Lock()
	fake.beforeNamespaceGet = func() { fake.beforeNamespaceGet = func() { fake.atespaces[name].Metadata.Uid = "replacement-uid" } }
	fake.mu.Unlock()
	r, b, _ := ts.do(admin, "GET", "/api/admin/orphan-swarms/"+recoveryTestID+"?atespace_uid=expected-uid", "", nil)
	expectStatus(t, "native replacement during inspection", r, b, 409)
}
