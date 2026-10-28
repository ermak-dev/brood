package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const recoveryTestID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// FEATURE: Explicit administrator recovery of an untracked native namespace (#68).
// SPEC: UID-fenced, strict input, no native mutation or overwrite, no ordinary
// user access, and current administrator privileges checked at the SQL write.
// IMPLEMENTATION: Read-only native discovery plus explicit transactional local recovery.
// RESULT: IMPLEMENTED — UID/name/state, strict body, stale grants and conflicting writes tested.
func addRecoveryNamespace(fake *fakeAteapi, name, uid string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.atespaces[name] = &ateapi.Atespace{Metadata: &ateapi.ResourceMetadata{Name: name, Uid: uid, Version: 7}}
}

func recoveryReads(fake *fakeAteapi) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.namespaceReads
}

func TestOrphanRecoveryRequiresAdminBeforeUpstream(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, alice, _, _ := actorsSetup(t, ts.testServer)
	anonymous := newClient(t)
	ts.do(anonymous, "GET", "/api/auth/session", "", nil)
	before := recoveryReads(fake)
	for _, client := range []struct {
		name   string
		client *http.Client
		status int
	}{{"ordinary", alice, 403}, {"anonymous", anonymous, 401}} {
		for _, method := range []string{"GET", "POST"} {
			url := "/api/admin/orphan-swarms"
			if method == "POST" {
				url += "/recover"
			}
			r, body, _ := ts.do(client.client, method, url, ts.csrf(client.client), map[string]string{})
			expectStatus(t, client.name+" "+method, r, body, client.status)
		}
	}
	if got := recoveryReads(fake); got != before {
		t.Fatalf("unauthorized recovery reached native API: %d -> %d", before, got)
	}
	r, body, _ := ts.do(admin, "POST", "/api/admin/orphan-swarms/recover", "", map[string]string{})
	expectStatus(t, "csrf", r, body, 403)
}

func TestOrphanRecoveryIsExplicitAndPreservesNativeResources(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, _, _, knownID := actorsSetup(t, ts.testServer)
	known, err := ts.srv.store.GetSwarm(context.Background(), knownID)
	if err != nil {
		t.Fatal(err)
	}
	// Even a deleted database row is a known binding, not an orphan candidate.
	if err := ts.srv.store.SetSwarmState(context.Background(), knownID, "deleted"); err != nil {
		t.Fatal(err)
	}
	name := app.AtespaceName(recoveryTestID)
	addRecoveryNamespace(fake, name, "expected-uid")
	addRecoveryNamespace(fake, "operator-space", "unrelated-uid")
	r, body, raw := ts.do(admin, "GET", "/api/admin/orphan-swarms", "", nil)
	expectStatus(t, "candidates", r, body, 200)
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("candidates = %v", body)
	}
	candidate := items[0].(map[string]any)
	if candidate["atespace_name"] != name || candidate["atespace_uid"] != "expected-uid" {
		t.Fatalf("wrong orphan candidate: %v", candidate)
	}
	if strings.Contains(raw, "test-token") || strings.Contains(raw, "BEGIN CERTIFICATE") {
		t.Fatal("candidate response disclosed infrastructure credentials")
	}
	request := map[string]string{"project_id": known.ProjectID, "swarm_id": recoveryTestID, "atespace_uid": "expected-uid"}
	r, body, _ = ts.do(admin, "POST", "/api/admin/orphan-swarms/recover", ts.csrf(admin), request)
	expectStatus(t, "recovery", r, body, 201)
	if body["id"] != recoveryTestID || body["owner_username"] != "root" || body["role"] != "owner" || body["atespace_name"] != name || body["state"] != "active" {
		t.Fatalf("wrong recovered record: %v", body)
	}
	saved, err := ts.srv.store.GetSwarm(context.Background(), recoveryTestID)
	if err != nil || saved.AtespaceUID == nil || *saved.AtespaceUID != "expected-uid" {
		t.Fatalf("UID was not preserved: %+v, %v", saved, err)
	}
	root, err := ts.srv.store.GetUserByUsername(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	if saved.OwnerID != root.ID {
		t.Fatalf("owner = %q, want acting admin", saved.OwnerID)
	}
	if role, err := ts.srv.store.GetSwarmRole(context.Background(), saved.ID, root.ID); err != nil || role != "owner" {
		t.Fatalf("recovered owner grant = %q, %v", role, err)
	}
	fake.mu.Lock()
	count, uid, version := len(fake.atespaces), fake.atespaces[name].Metadata.Uid, fake.atespaces[name].Metadata.Version
	fake.mu.Unlock()
	if count != 3 || uid != "expected-uid" || version != 7 {
		t.Fatalf("recovery changed native data: count=%d uid=%q version=%d", count, uid, version)
	}
	r, body, _ = ts.do(admin, "POST", "/api/admin/orphan-swarms/recover", ts.csrf(admin), request)
	expectStatus(t, "existing binding", r, body, 409)
	r, body, _ = ts.do(admin, "GET", "/api/admin/orphan-swarms", "", nil)
	expectStatus(t, "no remaining candidates", r, body, 200)
	if len(body["items"].([]any)) != 0 {
		t.Fatalf("recovered namespace remains an orphan candidate: %v", body)
	}
}

func TestOrphanRecoveryRejectsInvalidClaimsWithoutRecords(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, _, _, knownID := actorsSetup(t, ts.testServer)
	known, err := ts.srv.store.GetSwarm(context.Background(), knownID)
	if err != nil {
		t.Fatal(err)
	}
	addRecoveryNamespace(fake, app.AtespaceName(recoveryTestID), "expected-uid")
	for _, tc := range []struct {
		name, field, value string
		status             int
	}{
		{"UID mismatch", "atespace_uid", "wrong-uid", 409},
		{"UID required", "atespace_uid", "", 400},
		{"missing project", "project_id", "missing", 404},
		{"invalid ID", "swarm_id", "../../foreign", 400},
		{"unicode ID", "swarm_id", "01ARZ3NDEKTSV4RRFFQ69G5FAV", 400},
		{"unknown owner override", "owner_username", "alice", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]string{"project_id": known.ProjectID, "swarm_id": recoveryTestID, "atespace_uid": "expected-uid"}
			request[tc.field] = tc.value
			r, body, _ := ts.do(admin, "POST", "/api/admin/orphan-swarms/recover", ts.csrf(admin), request)
			expectStatus(t, tc.name, r, body, tc.status)
			if _, err := ts.srv.store.GetSwarm(context.Background(), recoveryTestID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refused recovery created a row: %v", err)
			}
		})
	}
}

func TestOrphanRecoveryRejectsTrailingJSONBeforeUpstream(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, _, _, knownID := actorsSetup(t, ts.testServer)
	known, err := ts.srv.store.GetSwarm(context.Background(), knownID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{"project_id": known.ProjectID, "swarm_id": recoveryTestID, "atespace_uid": "expected-uid"})
	if err != nil {
		t.Fatal(err)
	}
	before := recoveryReads(fake)
	request, err := http.NewRequest("POST", ts.http.URL+"/api/admin/orphan-swarms/recover", strings.NewReader(string(data)+" {}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", ts.http.URL)
	request.Header.Set("X-CSRFToken", ts.csrf(admin))
	response, err := admin.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 || recoveryReads(fake) != before {
		t.Fatalf("trailing JSON reached recovery: status=%d", response.StatusCode)
	}
}

func TestOrphanRecoveryRechecksLocalBindingsAfterNativeRead(t *testing.T) {
	for _, scenario := range []string{"project deleted", "binding claimed", "native name mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newFakeAteapi()
			ts := newSubstrateServer(t, fake)
			admin, _, _, knownID := actorsSetup(t, ts.testServer)
			known, err := ts.srv.store.GetSwarm(context.Background(), knownID)
			if err != nil {
				t.Fatal(err)
			}
			projectID := known.ProjectID
			if scenario == "project deleted" {
				projectID = "recovery-target"
				if err := ts.srv.store.CreateProject(context.Background(), store.Project{ID: projectID, Name: projectID}); err != nil {
					t.Fatal(err)
				}
			}
			alice, err := ts.srv.store.GetUserByUsername(context.Background(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			name := app.AtespaceName(recoveryTestID)
			addRecoveryNamespace(fake, name, "expected-uid")
			fake.mu.Lock()
			if scenario == "native name mismatch" {
				fake.atespaces[name].Metadata.Name = "operator-space"
			} else {
				fake.beforeNamespaceGet = func() {
					var err error
					if scenario == "project deleted" {
						err = ts.srv.store.DeleteProjectWithoutSwarms(context.Background(), projectID)
					} else {
						uid := "expected-uid"
						err = ts.srv.store.CreateSwarm(context.Background(), store.Swarm{ID: recoveryTestID, ProjectID: projectID,
							OwnerID: alice.ID, Name: "claimed", AtespaceName: name, AtespaceUID: &uid, State: "active"})
					}
					if err != nil {
						t.Errorf("interleaved mutation: %v", err)
					}
				}
			}
			fake.mu.Unlock()
			r, body, _ := ts.do(admin, "POST", "/api/admin/orphan-swarms/recover", ts.csrf(admin), map[string]string{
				"project_id": projectID, "swarm_id": recoveryTestID, "atespace_uid": "expected-uid",
			})
			want := 409
			if scenario == "project deleted" {
				want = 404
			}
			expectStatus(t, scenario, r, body, want)
			row, err := ts.srv.store.GetSwarm(context.Background(), recoveryTestID)
			if scenario == "binding claimed" {
				if err != nil || row.OwnerID != alice.ID || row.Name != "claimed" {
					t.Fatalf("existing binding overwritten: %+v, %v", row, err)
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refused recovery created row: %+v, %v", row, err)
			}
		})
	}
}

func TestOrphanRecoveryRefusesTerminatingNamespace(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, _, _, knownID := actorsSetup(t, ts.testServer)
	known, err := ts.srv.store.GetSwarm(context.Background(), knownID)
	if err != nil {
		t.Fatal(err)
	}
	name := app.AtespaceName(recoveryTestID)
	addRecoveryNamespace(fake, name, "expected-uid")
	fake.mu.Lock()
	fake.atespaces[name].Metadata.DeleteTime = timestamppb.Now()
	fake.mu.Unlock()
	r, body, _ := ts.do(admin, "GET", "/api/admin/orphan-swarms", "", nil)
	expectStatus(t, "terminating candidates", r, body, 200)
	if len(body["items"].([]any)) != 0 {
		t.Error("terminating namespace was listed as a recovery candidate")
	}
	r, body, _ = ts.do(admin, "POST", "/api/admin/orphan-swarms/recover", ts.csrf(admin), map[string]string{
		"project_id": known.ProjectID, "swarm_id": recoveryTestID, "atespace_uid": "expected-uid",
	})
	expectStatus(t, "terminating namespace refused", r, body, 409)
	if _, err := ts.srv.store.GetSwarm(context.Background(), recoveryTestID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("terminating namespace was adopted: %v", err)
	}
}

func TestOrphanRecoveryRechecksAdministratorAtWrite(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	admin, _, _, knownID := actorsSetup(t, ts.testServer)
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
	addRecoveryNamespace(fake, app.AtespaceName(recoveryTestID), "expected-uid")
	fake.mu.Lock()
	fake.beforeNamespaceGet = func() {
		if err := ts.srv.store.SetUserAdmin(context.Background(), root.ID, false); err != nil {
			t.Errorf("revoke administrator: %v", err)
		}
	}
	fake.mu.Unlock()
	r, body, _ := ts.do(admin, "POST", "/api/admin/orphan-swarms/recover", ts.csrf(admin), map[string]string{
		"project_id": known.ProjectID, "swarm_id": recoveryTestID, "atespace_uid": "expected-uid",
	})
	expectStatus(t, "stale administrator", r, body, 403)
	if _, err := ts.srv.store.GetSwarm(context.Background(), recoveryTestID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale administrator created a row: %v", err)
	}
}
