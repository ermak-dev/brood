package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// managedOriginSetup — managedSetup with installation remotes and the
// project repository of the returned swarm selected.
func managedOriginSetup(t *testing.T, repository string) (*substrateServer, *http.Client, string, *managedFakePVE, *managedFakeRelay) {
	t.Helper()
	ts, alice, _, swarm, pve, hub := managedSetup(t)
	pve.nextIDs = []int{105, 106}
	ts.srv.git.remotes = gitTestRemotes()
	setProjectRepository(t, ts, swarm, repository)
	return ts, alice, swarm, pve, hub
}

func setProjectRepository(t *testing.T, ts *substrateServer, swarm, repository string) {
	t.Helper()
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetProjectRepository(ctx, sw.ProjectID, repository); err != nil {
		t.Fatal(err)
	}
}

func managedOriginBody(name string, origin any) map[string]any {
	body := map[string]any{"metadata": map[string]string{"name": name}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}}
	if origin != nil {
		body["origin"] = origin
	}
	return body
}

// FEATURE: Managed actor origin at creation (#146).
// SPEC: An explicit origin is canonicalized and must be gateway (exactly the
// project repository and an installation remote) or direct (a public guest
// host); anything else is refused with 422 before any reservation. The
// canonical origin persists; JSON exposes origin and the current originMode.
// IMPLEMENTATION: Resolution precedes admission; mode is recomputed on read.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestManagedActorOriginCreation(t *testing.T) {
	ts, alice, swarm, pve, _ := managedOriginSetup(t, gitTestOrigin)
	path := "/api/swarms/" + swarm + "/actors"
	for _, origin := range []string{
		"https://git.example.com/group/other.git",
		"https://unlisted.example/repo",
		"https://brood-bot:" + gitTokenSentinel + "@git.example.com/group/repo.git",
		"http://github.com/owner/public",
		"https://192.168.1.66/group/repo.git",
	} {
		r, _, raw := ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("refused", origin))
		if r.StatusCode != http.StatusUnprocessableEntity || strings.Contains(raw, gitTokenSentinel) || strings.Contains(raw, origin) || !strings.Contains(raw, "origin") {
			t.Fatalf("origin %q: status=%d body=%s", origin, r.StatusCode, raw)
		}
	}
	for _, body := range []string{
		`{"metadata":{"name":"refused"},"actorTemplate":{"name":"claude-proxmox-brood-proxmox"},"origin":1}`,
		`{"metadata":{"name":"refused"},"actorTemplate":{"name":"claude-proxmox-brood-proxmox"},"origin":null}`,
		`{"metadata":{"name":"refused"},"actorTemplate":{"name":"claude-proxmox-brood-proxmox"},"origin":"https://github.com/a/b","origin":"https://github.com/c/d"}`,
		`{"metadata":{"name":"refused"},"actorTemplate":{"name":"claude-proxmox-brood-proxmox"},"Origin":"https://github.com/a/b"}`,
	} {
		r, _, raw := ts.doRaw(alice, "POST", path, ts.csrf(alice), body)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: status=%d %s", body, r.StatusCode, raw)
		}
	}
	if rows, err := ts.srv.store.ListManagedActors(context.Background(), swarm); err != nil || len(rows) != 0 || pve.creates != 0 {
		t.Fatalf("refused origin reserved: %v %v creates=%d", rows, err, pve.creates)
	}
	r, b, raw := ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("gateway", "https://Git.Example.com/group/repo.git/"))
	expectStatus(t, "gateway", r, b, http.StatusCreated)
	if b["origin"] != gitTestOrigin || b["originMode"] != "gateway" || strings.Contains(raw, gitTokenSentinel) {
		t.Fatalf("gateway actor: %s", raw)
	}
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("direct", "https://github.com/owner/public"))
	expectStatus(t, "direct", r, b, http.StatusCreated)
	if b["origin"] != "https://github.com/owner/public" || b["originMode"] != "direct" {
		t.Fatalf("direct actor: %v", b)
	}
	gateway, err := ts.srv.store.LookupManagedActor(context.Background(), swarm, "gateway")
	if err != nil || gateway.Origin != gitTestOrigin {
		t.Fatalf("canonical origin not durable: %+v %v", gateway, err)
	}
	setProjectRepository(t, ts, swarm, "")
	r, b, _ = ts.do(alice, "GET", path+"/gateway", "", nil)
	expectStatus(t, "read", r, b, http.StatusOK)
	if b["origin"] != gitTestOrigin || b["originMode"] != "" {
		t.Fatalf("mode not recomputed: %v", b)
	}
	r, b, raw = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "list", r, b, http.StatusOK)
	modes := map[string][2]any{}
	for _, item := range b["results"].([]any) {
		row := item.(map[string]any)
		modes[row["metadata"].(map[string]any)["name"].(string)] = [2]any{row["origin"], row["originMode"]}
	}
	if modes["direct"] != [2]any{"https://github.com/owner/public", "direct"} || modes["gateway"] != [2]any{gitTestOrigin, ""} || strings.Contains(raw, gitTokenSentinel) {
		t.Fatalf("list origin modes: %v", modes)
	}
}

func TestManagedActorOriginDefaults(t *testing.T) {
	ts, alice, swarm, _, _ := managedOriginSetup(t, gitTestOrigin)
	path := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("repository", nil))
	expectStatus(t, "default repository", r, b, http.StatusCreated)
	if b["origin"] != gitTestOrigin || b["originMode"] != "gateway" {
		t.Fatalf("project repository default: %v", b)
	}
	setProjectRepository(t, ts, swarm, "")
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("empty", ""))
	expectStatus(t, "empty default", r, b, http.StatusCreated)
	if b["origin"] != "" || b["originMode"] != "" {
		t.Fatalf("empty default: %v", b)
	}
}

func TestManagedActorOriginRefusedForUnconfiguredRepositoryDefault(t *testing.T) {
	ts, alice, swarm, pve, _ := managedOriginSetup(t, gitTestOrigin)
	ts.srv.git.remotes = gitRemotes{}
	r, _, raw := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), managedOriginBody("stale", nil))
	if r.StatusCode != http.StatusUnprocessableEntity || pve.creates != 0 {
		t.Fatalf("unconfigured default repository: %d %s creates=%d", r.StatusCode, raw, pve.creates)
	}
}

func TestNativeActorRefusesOrigin(t *testing.T) {
	ts, alice, swarm, _, _ := managedOriginSetup(t, gitTestOrigin)
	body := map[string]any{"metadata": map[string]string{"name": "native"}, "actorTemplate": map[string]string{"name": "native-template"}, "origin": "https://github.com/owner/public"}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), body)
	expectStatus(t, "native origin", r, b, http.StatusBadRequest)
	expectDetail(t, "native origin", b, "origin is supported only for managed actors")
}

// managedOriginMaster creates a running approved master with the given origin
// while the project repository is the given one.
func managedOriginMaster(t *testing.T, origin, repository string) (*substrateServer, store.ManagedActor, *managedFakePVE) {
	t.Helper()
	ts, alice, swarm, pve, hub := managedOriginSetup(t, repository)
	ctx := context.Background()
	u, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetUserAdmin(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	body := managedOriginBody("master", origin)
	body["role"] = "master"
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), body)
	expectStatus(t, "master", r, b, http.StatusCreated)
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	for range 4 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	master, err := ts.srv.store.LookupManagedActor(ctx, swarm, "master")
	if err != nil || master.Phase != "running" || master.Origin != origin {
		t.Fatalf("master: %+v %v", master, err)
	}
	return ts, master, pve
}

func projectOf(t *testing.T, ts *substrateServer, swarm string) string {
	t.Helper()
	sw, err := ts.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil {
		t.Fatal(err)
	}
	return sw.ProjectID
}

// SPEC: A worker added by a master inherits the master's origin unless the
// add names one; the guest catalog add accepts --origin.
func TestManagedMasterAddOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, master string
		request      relay.SwarmRequest
		want, mode   string
	}{
		{"legacy inherits", gitTestOrigin, relay.SwarmRequest{Tool: "add", To: "child"}, gitTestOrigin, "gateway"},
		{"catalog inherits", gitTestOrigin, relay.SwarmRequest{Tool: "add", Arguments: json.RawMessage(`{"name":"child"}`)}, gitTestOrigin, "gateway"},
		{"explicit direct", gitTestOrigin, relay.SwarmRequest{Tool: "add", Arguments: json.RawMessage(`{"name":"child","origin":"https://github.com/owner/public/"}`)}, "https://github.com/owner/public", "direct"},
		{"empty master uses repository", "", relay.SwarmRequest{Tool: "add", Arguments: json.RawMessage(`{"name":"child"}`)}, gitTestOrigin, "gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := gitTestOrigin
			if tc.master == "" {
				repository = ""
			}
			ts, master, _ := managedOriginMaster(t, tc.master, repository)
			setProjectRepository(t, ts, master.SwarmID, gitTestOrigin)
			got := ts.srv.managed.handleSwarmRequest(context.Background(), master.UID, master.Incarnation, tc.request)
			if !got.OK {
				t.Fatalf("add refused: %+v", got)
			}
			child, err := ts.srv.store.LookupManagedActor(context.Background(), master.SwarmID, "child")
			if err != nil || child.Origin != tc.want || child.ParentUID != master.UID {
				t.Fatalf("child: %+v %v", child, err)
			}
			mode, err := ts.srv.originMode(context.Background(), projectOf(t, ts, master.SwarmID), child.Origin, child.NetworkPolicy)
			if err != nil || mode != tc.mode {
				t.Fatalf("child mode %q %v; want %q", mode, err, tc.mode)
			}
		})
	}
}

func TestManagedMasterAddOriginRefusal(t *testing.T) {
	ts, master, pve := managedOriginMaster(t, gitTestOrigin, gitTestOrigin)
	creates := pve.creates
	for _, origin := range []string{"https://git.example.com/group/other.git", "https://brood-bot:" + gitTokenSentinel + "@github.com/owner/public"} {
		args, _ := json.Marshal(map[string]string{"name": "child", "origin": origin})
		got := ts.srv.managed.handleSwarmRequest(context.Background(), master.UID, master.Incarnation, relay.SwarmRequest{Tool: "add", Arguments: args})
		if got.OK || !strings.Contains(got.Text, "origin") || strings.Contains(got.Text, gitTokenSentinel) || strings.Contains(got.Text, origin) {
			t.Fatalf("invalid child origin: %+v", got)
		}
	}
	if _, err := ts.srv.store.LookupManagedActor(context.Background(), master.SwarmID, "child"); !errors.Is(err, store.ErrNotFound) || pve.creates != creates {
		t.Fatalf("refused origin reserved: %v creates=%d", err, pve.creates)
	}
}

func TestManagedMasterAddRetryKeepsOrigin(t *testing.T) {
	ts, master, _ := managedOriginMaster(t, gitTestOrigin, gitTestOrigin)
	ctx := context.Background()
	add := func(arguments string) relay.SwarmResponse {
		return ts.srv.managed.handleSwarmRequest(ctx, master.UID, master.Incarnation, relay.SwarmRequest{Tool: "add", Arguments: json.RawMessage(arguments)})
	}
	if got := add(`{"name":"child","origin":"https://github.com/owner/public"}`); !got.OK {
		t.Fatalf("add: %+v", got)
	}
	if got := add(`{"name":"child","origin":"https://github.com/owner/public"}`); !got.OK {
		t.Fatalf("identical retry: %+v", got)
	}
	if got := add(`{"name":"child"}`); !got.OK {
		t.Fatalf("name-only retry: %+v", got)
	}
	if got := add(`{"name":"child","origin":"https://github.com/owner/other"}`); got.OK {
		t.Fatalf("conflicting retry accepted: %+v", got)
	}
	child, err := ts.srv.store.LookupManagedActor(ctx, master.SwarmID, "child")
	if err != nil || child.Origin != "https://github.com/owner/public" {
		t.Fatalf("retry changed origin: %+v %v", child, err)
	}
}

// doRaw sends an exact JSON body, including duplicate keys json.Marshal cannot express.
func (ts *testServer) doRaw(client *http.Client, method, path, token, body string) (*http.Response, map[string]any, string) {
	ts.t.Helper()
	req := httptest.NewRequest(method, ts.http.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.http.URL)
	if token != "" {
		req.Header.Set(auth.HeaderCSRF, token)
	}
	for _, cookie := range client.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	var data map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	return w.Result(), data, w.Body.String()
}
