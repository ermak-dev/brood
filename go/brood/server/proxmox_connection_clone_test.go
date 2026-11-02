package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"

	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

type cloneRevokingBody struct {
	io.Reader
	revoke func()
}

func (b *cloneRevokingBody) Read(p []byte) (int, error) {
	if b.revoke != nil {
		f := b.revoke
		b.revoke = nil
		f()
	}
	return b.Reader.Read(p)
}

func TestProxmoxConnectionCloneFreshAuthorization(t *testing.T) {
	for _, action := range []string{"demote", "disable", "revoke"} {
		t.Run(action, func(t *testing.T) {
			ts, _, _, _, _, _ := managedSetup(t)
			admin := newClient(t)
			ts.login(admin, "root", "root-password-1")
			uid := cloneSource(t, ts, admin)
			csrf := ts.csrf(admin)
			cloneSpareAdmin(t, ts)
			root, err := ts.srv.store.GetUserByUsername(context.Background(), "root")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(cloneRuntimeBody())
			body := &cloneRevokingBody{Reader: strings.NewReader(string(raw)), revoke: func() {
				var err error
				if action == "revoke" {
					_, err = ts.srv.store.DeleteSessionsOfUser(context.Background(), root.ID)
				} else {
					err = ts.srv.store.UpdateUserProperties(context.Background(), root.ID, "Root", action != "demote", action == "disable")
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			req := httptest.NewRequest("POST", ts.http.URL+"/api/proxmox-connections/"+uid+"/clone", body)
			u, _ := url.Parse(ts.http.URL)
			for _, cookie := range admin.Jar.Cookies(u) {
				req.AddCookie(cookie)
			}
			req.Header.Set("Origin", ts.http.URL)
			req.Header.Set(auth.HeaderCSRF, csrf)
			w := httptest.NewRecorder()
			ts.srv.Handler().ServeHTTP(w, req)
			if w.Code != 401 && w.Code != 403 {
				t.Fatalf("stale authorization accepted: %d", w.Code)
			}
			rows, _ := ts.srv.store.ListManagedConnections(context.Background())
			if len(rows) != 1 {
				t.Fatal("stale authorization persisted clone")
			}
		})
	}
}

func cloneSpareAdmin(t *testing.T, ts *substrateServer) {
	t.Helper()
	bob, err := ts.srv.store.GetUserByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.UpdateUserProperties(context.Background(), bob.ID, "Bob", true, false); err != nil {
		t.Fatal(err)
	}
}

func cloneRuntimeBody() map[string]any {
	return map[string]any{"name": "image-acceptance", "template": "local:vztmpl/brood-claude-proxmox-bbbbbbbb.tar.zst", "templateDigest": "sha256:" + strings.Repeat("b", 64), "capacity": map[string]any{"actors": 1, "cpuMillicores": "2000", "memoryBytes": "2147483648", "diskGiB": 8}}
}

func cloneSource(t *testing.T, ts *substrateServer, admin *http.Client) string {
	t.Helper()
	_, ca := selfSignFake(t, "clone-source")
	body := connectionBody("clone-source", string(ca))
	for k, v := range cloneRuntimeBody() {
		if k != "name" {
			body[k] = v
		}
	}
	body["templateDigest"] = "sha256:" + strings.Repeat("a", 64)
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-connections", ts.csrf(admin), body)
	expectStatus(t, "source", r, b, 201)
	return b["uid"].(string)
}

func TestProxmoxConnectionClone(t *testing.T) {
	ts, alice, _, swarm, _, _ := managedSetup(t)
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	uid := cloneSource(t, ts, admin)
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-pools/clone-source/activate", ts.csrf(admin), nil)
	expectStatus(t, "source activation", r, b, 201)
	r, b, _ = ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "occupied"}, "actorTemplate": map[string]string{"name": "claude-proxmox-clone-source"}})
	expectStatus(t, "source actor", r, b, 201)
	ctx := context.Background()
	before, _ := ts.srv.store.ListManagedConnections(ctx)
	pools, _ := ts.srv.store.ListManagedPools(ctx)
	actors, _ := ts.srv.store.ListManagedActors(ctx, swarm)
	if len(actors) != 1 {
		t.Fatal("missing occupied fixture")
	}
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { t.Fatal("clone contacted PVE"); return nil, nil }
	r, b, raw := ts.do(admin, "POST", "/api/proxmox-connections/"+uid+"/clone", ts.csrf(admin), cloneRuntimeBody())
	expectStatus(t, "clone", r, b, 201)
	rows, err := ts.srv.store.ListManagedConnections(ctx)
	if err != nil || len(rows) != len(before)+1 {
		t.Fatal("clone did not add exactly one connection")
	}
	var cloned store.ManagedConnection
	for _, c := range rows {
		if c.Name == "image-acceptance" {
			cloned = c
		}
	}
	if cloned.UID == "" || cloned.UID == uid {
		t.Fatal("clone needs an independent UID")
	}
	var source store.ManagedConnection
	for _, c := range before {
		if c.UID == uid {
			source = c
		}
	}
	expected := source
	expected.UID = cloned.UID
	expected.Name = cloned.Name
	expected.CreatedAt = cloned.CreatedAt
	expected.UpdatedAt = cloned.UpdatedAt
	expected.RuntimeJSON = cloned.RuntimeJSON
	if !reflect.DeepEqual(expected, cloned) {
		t.Fatal("inherited connection metadata differs")
	}
	secret, err := ts.srv.store.ManagedConnectionSecret(ctx, cloned.Name)
	if err != nil || secret != "form-secret-value" {
		t.Fatal("secret not inherited internally")
	}
	runtime, err := connectionConfig(cloned)
	if err != nil || runtime.runtime == nil || runtime.runtime.digest != cloneRuntimeBody()["templateDigest"] || runtime.runtime.template != cloneRuntimeBody()["template"] || runtime.runtime.actors != 1 || runtime.runtime.cpuMillicores != 2000 || runtime.runtime.memoryBytes != 2147483648 || runtime.runtime.diskGiB != 8 {
		t.Fatal("new runtime not persisted")
	}
	if !reflect.DeepEqual(b, connectionDTO(cloned)) {
		t.Fatal("clone response is not standard metadata DTO")
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, "BEGIN CERTIFICATE") || b["caPEM"] != nil || b["tokenSecret"] != nil {
		t.Fatal("credential returned")
	}
	for _, old := range before {
		var found bool
		for _, c := range rows {
			if c.UID == old.UID {
				found = reflect.DeepEqual(c, old)
			}
		}
		if !found {
			t.Fatal("existing connection changed")
		}
	}
	afterPools, _ := ts.srv.store.ListManagedPools(ctx)
	afterActors, _ := ts.srv.store.ListManagedActors(ctx, swarm)
	if !reflect.DeepEqual(pools, afterPools) || !reflect.DeepEqual(actors, afterActors) {
		t.Fatal("occupied pool or actor changed")
	}
	sourceSecret, _ := ts.srv.store.ManagedConnectionSecret(ctx, source.Name)
	if sourceSecret != secret {
		t.Fatal("source secret changed")
	}
	r, b, raw = ts.do(admin, "GET", "/api/proxmox-connections", "", nil)
	expectStatus(t, "list", r, b, 200)
	if strings.Contains(raw, secret) || strings.Contains(raw, "BEGIN CERTIFICATE") {
		t.Fatal("list leaked credentials")
	}
}

func TestProxmoxConnectionCloneValidation(t *testing.T) {
	ts, _, _, _, _, _ := managedSetup(t)
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	uid := cloneSource(t, ts, admin)
	path := "/api/proxmox-connections/" + uid + "/clone"
	before, _ := ts.srv.store.ListManagedConnections(context.Background())
	cases := []struct {
		name   string
		body   any
		status int
	}{
		{"missing name", map[string]any{"template": cloneRuntimeBody()["template"], "templateDigest": cloneRuntimeBody()["templateDigest"], "capacity": cloneRuntimeBody()["capacity"]}, 422},
		{"empty", map[string]any{}, 422},
		{"no runtime", map[string]any{"name": "read-only"}, 422},
		{"no template", map[string]any{"name": "invalid", "templateDigest": cloneRuntimeBody()["templateDigest"], "capacity": cloneRuntimeBody()["capacity"]}, 422},
		{"no digest", map[string]any{"name": "invalid", "template": cloneRuntimeBody()["template"], "capacity": cloneRuntimeBody()["capacity"]}, 422},
	}
	for _, field := range []string{"name", "template", "templateDigest", "capacity"} {
		for _, value := range []any{nil, true, 42, []any{}} {
			body := cloneRuntimeBody()
			body[field] = value
			cases = append(cases, struct {
				name   string
				body   any
				status int
			}{field + " type", body, 422})
		}
	}
	for _, field := range []string{"endpoint", "tokenID", "tokenSecret", "caPEM", "node", "resourcePool", "storage", "network", "uid", "credentialRef", "Name", "Template", "TemplateDigest", "Capacity"} {
		body := cloneRuntimeBody()
		body[field] = "override-secret"
		cases = append(cases, struct {
			name   string
			body   any
			status int
		}{field, body, 400})
	}
	for _, value := range []any{json.RawMessage(`{"actors":1,"actors":2,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":8}`), json.RawMessage(`{"Actors":1,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":8}`), json.RawMessage(`{"actors":1,"cpuMillicores":2000,"memoryBytes":"2147483648","diskGiB":8}`), json.RawMessage(`{"actors":1,"cpuMillicores":"9223372036854775808","memoryBytes":"2147483648","diskGiB":8}`), json.RawMessage(`{"actors":1,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":9223372036854775807}`), json.RawMessage(`{"actors":0,"cpuMillicores":"2000","memoryBytes":"2147483648","diskGiB":8}`)} {
		body := cloneRuntimeBody()
		body["capacity"] = value
		cases = append(cases, struct {
			name   string
			body   any
			status int
		}{"capacity", body, 422})
	}
	for _, field := range []string{"name", "template", "templateDigest"} {
		body := cloneRuntimeBody()
		body[field] = "invalid/value"
		cases = append(cases, struct {
			name   string
			body   any
			status int
		}{"invalid " + field, body, 422})
	}
	for _, name := range []string{"clone-source", "brood-proxmox"} {
		body := cloneRuntimeBody()
		body["name"] = name
		cases = append(cases, struct {
			name   string
			body   any
			status int
		}{"collision", body, 409})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, b, raw := ts.do(admin, "POST", path, ts.csrf(admin), tc.body)
			expectStatus(t, "reject", r, b, tc.status)
			if strings.Contains(raw, "form-secret-value") || strings.Contains(raw, "override-secret") || strings.Contains(raw, "BEGIN CERTIFICATE") {
				t.Fatal("error leaked input")
			}
		})
	}
	for _, payload := range []string{`null`, `[]`, `{} {}`, `{"name":"one","name":"two"}`, `{"name":"one","name":"two"}`, strings.Repeat(" ", 1<<20) + `{}`} {
		req, _ := http.NewRequest("POST", ts.http.URL+path, strings.NewReader(payload))
		req.Header.Set("Origin", ts.http.URL)
		req.Header.Set("X-CSRFToken", ts.csrf(admin))
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("bad JSON status %d", resp.StatusCode)
		}
	}
	for _, source := range []string{"missing", "brood-proxmox"} {
		r, b, _ := ts.do(admin, "POST", "/api/proxmox-connections/"+source+"/clone", ts.csrf(admin), cloneRuntimeBody())
		expectStatus(t, "missing DB UID", r, b, 404)
	}
	after, _ := ts.srv.store.ListManagedConnections(context.Background())
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed requests changed catalog")
	}
}

func TestProxmoxConnectionCloneAuthorization(t *testing.T) {
	ts, _, bob, _, _, _ := managedSetup(t)
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	uid := cloneSource(t, ts, admin)
	path := "/api/proxmox-connections/" + uid + "/clone"
	raw, _ := json.Marshal(cloneRuntimeBody())
	for _, tc := range []struct {
		name         string
		client       *http.Client
		origin, csrf string
		status       int
	}{
		{"anonymous", newClient(t), ts.http.URL, "", 401},
		{"nonadmin", bob, ts.http.URL, ts.csrf(bob), 403},
		{"no csrf", admin, ts.http.URL, "", 403},
		{"bad csrf", admin, ts.http.URL, "bad", 403},
		{"no origin", admin, "", ts.csrf(admin), 403},
		{"foreign origin", admin, "https://foreign.invalid", ts.csrf(admin), 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", ts.http.URL+path, strings.NewReader(string(raw)))
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-CSRFToken", tc.csrf)
			resp, err := tc.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d: %s", resp.StatusCode, b)
			}
		})
	}
	cloneSpareAdmin(t, ts)
	root, err := ts.srv.store.GetUserByUsername(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.UpdateUserProperties(context.Background(), root.ID, "Root", false, false); err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(admin, "POST", path, ts.csrf(admin), cloneRuntimeBody())
	expectStatus(t, "demotion revoked session", r, b, 401)
	ts.login(admin, "root", "root-password-1")
	r, b, _ = ts.do(admin, "POST", path, ts.csrf(admin), cloneRuntimeBody())
	expectStatus(t, "demoted admin with new session", r, b, 403)
	rows, _ := ts.srv.store.ListManagedConnections(context.Background())
	if len(rows) != 1 {
		t.Fatal("unauthorized clone persisted")
	}
}
