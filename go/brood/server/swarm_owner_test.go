package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSwarmOwnerUsername(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	response, project, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "owners"})
	expectStatus(t, "project", response, project, http.StatusCreated)
	path := "/api/projects/" + project["id"].(string) + "/swarms"
	response, swarm, _ := ts.do(admin, "POST", path, ts.csrf(admin), map[string]string{"name": "team"})
	expectStatus(t, "create", response, swarm, http.StatusCreated)
	if swarm["owner_username"] != "root" {
		t.Fatalf("create owner: %v", swarm["owner_username"])
	}
	response, swarm, _ = ts.do(admin, "GET", "/api/swarms/"+swarm["id"].(string), "", nil)
	expectStatus(t, "get", response, swarm, http.StatusOK)
	if swarm["owner_username"] != "root" {
		t.Fatalf("get owner: %v", swarm["owner_username"])
	}
	response, _, raw := ts.do(admin, "GET", path, "", nil)
	var swarms []map[string]any
	if err := json.Unmarshal([]byte(raw), &swarms); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || len(swarms) != 1 || swarms[0]["owner_username"] != "root" {
		t.Fatalf("list: %d %s", response.StatusCode, raw)
	}
}
