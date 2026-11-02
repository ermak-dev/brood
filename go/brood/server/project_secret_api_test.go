package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/projectsecret"
)

// FEATURE: Project-secret management is write-only and administrator-scoped.
// SPEC: Metadata excludes values; batches are validated before mutation; runtime delivery binds an actor generation.
func TestProjectSecretAPIWriteOnlyAndAuthorization(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("admin", "admin-password-1")
	admin := newClient(t)
	ts.login(admin, "admin", "admin-password-1")
	r, b, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]any{"name": "secret-project"})
	expectStatus(t, "project", r, b, 201)
	id := b["id"].(string)
	path := "/api/projects/" + id + "/secrets"
	value := "synthetic-secret-value-not-a-credential"
	r, b, raw := ts.do(admin, "PUT", path+"/vars", ts.csrf(admin), map[string]any{"vars": map[string]string{"APP_TOKEN": value}})
	if r.StatusCode != 200 || strings.Contains(raw, value) {
		t.Fatalf("write leaked or failed: %d %s", r.StatusCode, raw)
	}
	r, b, raw = ts.do(admin, "GET", path, "", nil)
	if r.StatusCode != 200 || strings.Contains(raw, value) || strings.Contains(raw, `"data"`) || !strings.Contains(raw, "APP_TOKEN") {
		t.Fatalf("metadata leaked or failed: %d %s", r.StatusCode, raw)
	}
	r, _, raw = ts.do(admin, "GET", path+"/vars", "", nil)
	if r.StatusCode != 405 || strings.Contains(raw, value) {
		t.Fatalf("value read exposed: %d %s", r.StatusCode, raw)
	}
	anon := newClient(t)
	r, _, _ = ts.do(anon, "GET", path, "", nil)
	if r.StatusCode != 401 {
		t.Fatalf("anonymous metadata status=%d", r.StatusCode)
	}
	r, _, _ = ts.do(admin, "PUT", path+"/vars", "", map[string]any{"vars": map[string]string{"APP_OTHER": "synthetic-other"}})
	if r.StatusCode != 403 {
		t.Fatalf("csrf bypass status=%d", r.StatusCode)
	}
	files := []map[string]string{{"name": "valid.env", "data": base64.StdEncoding.EncodeToString([]byte(value))}, {"name": "../escape", "data": "eA=="}}
	r, _, raw = ts.do(admin, "PUT", path+"/files", ts.csrf(admin), map[string]any{"files": files})
	if r.StatusCode != 400 || strings.Contains(raw, value) {
		t.Fatalf("invalid batch: %d %s", r.StatusCode, raw)
	}
	meta, err := ts.srv.store.GetProjectSecretMetadata(context.Background(), id)
	if err != nil || len(meta.Files) != 0 || len(meta.Vars) != 1 {
		t.Fatalf("invalid batch mutated secrets: %+v %v", meta, err)
	}
}

func TestGuestSecretDeliveryBindsProjectAndGeneration(t *testing.T) {
	ts, admin, master, _ := masterSetup(t)
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	marker := "synthetic-actor-secret"
	if err := ts.srv.store.PutProjectSecretVars(ctx, sw.ProjectID, map[string]string{"APP_TOKEN": marker}); err != nil {
		t.Fatal(err)
	}
	token := guestToken(ts.srv.managed.registry.relaySecret, master.UID, master.Incarnation)
	path := fmt.Sprintf("/api/proxmox/secrets/%s/%d", master.UID, master.Incarnation)
	req, err := http.NewRequest("GET", ts.http.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var bundle projectsecret.Bundle
	if response.StatusCode != 200 {
		t.Fatalf("runtime fetch status=%d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Vars["APP_TOKEN"] != marker {
		t.Fatal("runtime did not receive its own project snapshot")
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("runtime snapshot is cacheable")
	}
	r, _, raw := ts.do(admin, "GET", path, "", nil)
	if r.StatusCode != 401 || strings.Contains(raw, marker) {
		t.Fatalf("operator cookie read runtime secret: %d %s", r.StatusCode, raw)
	}
}
