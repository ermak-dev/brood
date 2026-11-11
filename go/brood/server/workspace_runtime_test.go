package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// bootstrapEnvAtMain — закрытый список имён parseConfig brood-guest на main
// 61e09b8. Старый образ отказывает в загрузке на любое другое BROOD_GUEST_*
// имя, поэтому сервер не добавляет ни одного.
var bootstrapEnvAtMain = []string{"BROOD_GUEST_ACTOR_UID", "BROOD_GUEST_ALLOWED_HOSTS", "BROOD_GUEST_CA_PEM_B64", "BROOD_GUEST_GENERATION", "BROOD_GUEST_RELAY_HOST_KEY", "BROOD_GUEST_RELAY_TOKEN", "BROOD_GUEST_RELAY_URL"}

const workspaceTestDirect = "https://github.com/owner/public"

// FEATURE: Bootstrap env compatible with older guest images (#146).
// SPEC: Create and every start send exactly the bootstrap names of main, for an
// actor with a gateway origin, a direct origin and none; the origin and the
// installation credential never enter the env. The origin reaches the guest
// only through GET /api/proxmox/workspace/{uid}/{generation}.
// IMPLEMENTATION: createSpec is the bootstrap of main; the fake PVE records
// every env it was given.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestManagedBootstrapEnvKeysMatchMain(t *testing.T) {
	for _, tc := range []struct{ name, repository, origin string }{
		{"gateway", gitTestOrigin, gitTestOrigin},
		{"direct", gitTestOrigin, workspaceTestDirect},
		{"none", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, alice, swarm, pve, hub := managedOriginSetup(t, tc.repository)
			ctx := context.Background()
			path := "/api/swarms/" + swarm + "/actors"
			var origin any
			if tc.origin != "" {
				origin = tc.origin
			}
			r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("actor", origin))
			expectStatus(t, "create", r, b, http.StatusCreated)
			if b["origin"] != tc.origin {
				t.Fatalf("origin = %v; want %q", b["origin"], tc.origin)
			}
			hub.mu.Lock()
			hub.connected = true
			hub.mu.Unlock()
			for range 3 {
				ts.srv.managed.reconcileOnce(ctx)
			}
			for _, action := range []string{"stop", "resume"} {
				r, b, _ := ts.do(alice, "POST", path+"/actor", ts.csrf(alice), map[string]string{"action": action})
				expectStatus(t, action, r, b, http.StatusOK)
				for range 2 {
					ts.srv.managed.reconcileOnce(ctx)
				}
			}
			pve.mu.Lock()
			envs := append([]proxmox.BootstrapEnv(nil), pve.envs...)
			creates, bootstraps := pve.creates, pve.bootstraps
			pve.mu.Unlock()
			if creates != 1 || bootstraps != 2 || len(envs) != 3 {
				t.Fatalf("lifecycle: creates=%d bootstraps=%d envs=%d", creates, bootstraps, len(envs))
			}
			for i, env := range envs {
				keys := make([]string, 0, len(env))
				for key := range env {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				if !reflect.DeepEqual(keys, bootstrapEnvAtMain) {
					t.Fatalf("env %d keys %v; want the keys of main %v", i, keys, bootstrapEnvAtMain)
				}
				for key, value := range env {
					if (tc.origin != "" && strings.Contains(value, tc.origin)) || strings.Contains(value, gitTokenSentinel) || strings.Contains(value, "brood-bot") {
						t.Fatalf("env %d %s carries the origin or the installation Git credential", i, key)
					}
				}
			}
		})
	}
}

// workspaceFixture: "clone" (gateway origin, running) and "direct" (direct
// origin, durable start intent) in one swarm. The fake PVE models one CT and
// the pool admits two actors, so only "clone" runs there.
type workspaceFixture struct {
	ts            *substrateServer
	alice         *http.Client
	swarm         string
	clone, direct store.ManagedActor
}

func workspaceSetup(t *testing.T) workspaceFixture {
	t.Helper()
	ts, alice, swarm, _, hub := managedOriginSetup(t, gitTestOrigin)
	ctx := context.Background()
	path := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("clone", gitTestOrigin))
	expectStatus(t, "create clone", r, b, http.StatusCreated)
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	for range 3 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	f := workspaceFixture{ts: ts, alice: alice, swarm: swarm}
	var err error
	if f.clone, err = ts.srv.store.LookupManagedActor(ctx, swarm, "clone"); err != nil || f.clone.Phase != "running" {
		t.Fatalf("gateway actor: %+v %v", f.clone, err)
	}
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("direct", workspaceTestDirect))
	expectStatus(t, "create direct", r, b, http.StatusCreated)
	f.direct = workspaceStartIntent(t, ts, swarm, "direct")
	if f.direct.Origin != workspaceTestDirect {
		t.Fatalf("direct origin: %q", f.direct.Origin)
	}
	return f
}

// workspaceStartIntent settles a created actor as stopped and records a start
// intent: its guest token is then accepted without running a CT.
func workspaceStartIntent(t *testing.T, ts *substrateServer, swarm, name string) store.ManagedActor {
	t.Helper()
	ctx := context.Background()
	a, err := ts.srv.store.LookupManagedActor(ctx, swarm, name)
	if err == nil {
		a, err = ts.srv.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "create", "stopped")
	}
	if err == nil {
		a, err = ts.srv.store.BeginManagedActorOperation(ctx, a.UID, a.Revision, store.NewID(), "start")
	}
	if err != nil {
		t.Fatalf("%s start intent: %v", name, err)
	}
	return a
}

func (f workspaceFixture) token(a store.ManagedActor) string {
	return guestToken(f.ts.srv.managed.registry.relaySecret, a.UID, a.Incarnation)
}

func workspacePath(a store.ManagedActor) string {
	return fmt.Sprintf("/api/proxmox/workspace/%s/%d", a.UID, a.Incarnation)
}

func (f workspaceFixture) serve(method, path string, body io.Reader, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	for key, values := range header {
		req.Header[key] = values
	}
	w := httptest.NewRecorder()
	f.ts.srv.Handler().ServeHTTP(w, req)
	return w
}

// expectWorkspace requires 200 with exactly {"origin","mode"} and the headers
// of the secrets runtime route.
func expectWorkspace(t *testing.T, what string, w *httptest.ResponseRecorder, origin, mode string) {
	t.Helper()
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("%s: status=%d header=%v body=%s", what, w.Code, w.Header(), w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !reflect.DeepEqual(got, map[string]any{"origin": origin, "mode": mode}) {
		t.Fatalf("%s: body %s; want origin %q mode %q", what, w.Body.String(), origin, mode)
	}
	if strings.Contains(w.Body.String(), gitTokenSentinel) || strings.Contains(w.Body.String(), "brood-bot") {
		t.Fatalf("%s: installation Git credential delivered", what)
	}
}

// expectWorkspaceDenied requires the status without any origin, credential or token.
func expectWorkspaceDenied(t *testing.T, what string, w *httptest.ResponseRecorder, status int, f workspaceFixture) {
	t.Helper()
	body := w.Body.String()
	if w.Code != status || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("%s: status=%d want=%d header=%v body=%s", what, w.Code, status, w.Header(), body)
	}
	for _, secret := range []string{gitTestOrigin, workspaceTestDirect, "git.example.com", gitTokenSentinel, "brood-bot", f.token(f.clone), f.token(f.direct)} {
		if strings.Contains(body, secret) {
			t.Fatalf("%s: denied response leaked %q: %s", what, secret, body)
		}
	}
}

// FEATURE: Runtime workspace origin for the guest root supervisor (#146).
// SPEC: GET /api/proxmox/workspace/{uid}/{generation} returns the actor's
// canonical origin and its mode recomputed on this request, or both empty when
// the actor has no origin or the origin lost its mode. Authorization is exactly
// that of /api/proxmox/secrets: one Bearer guest token for the UID, generation
// and current start intent, an active swarm binding and namespace; cookies
// never authorize. The response is not cacheable.
// IMPLEMENTATION: The shared guest runtime authorization of the secrets route.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGuestWorkspaceOriginDelivery(t *testing.T) {
	f := workspaceSetup(t)
	clone, direct := workspacePath(f.clone), workspacePath(f.direct)
	expectWorkspace(t, "gateway", f.serve("GET", clone, nil, bearer(f.token(f.clone))), gitTestOrigin, "gateway")
	expectWorkspace(t, "direct", f.serve("GET", direct, nil, bearer(f.token(f.direct))), workspaceTestDirect, "direct")

	// Настоящий клиент через слушатель сервера: GET без тела и без query.
	req, err := http.NewRequest("GET", f.ts.http.URL+clone, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token(f.clone))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || string(raw) != `{"origin":"`+gitTestOrigin+`","mode":"gateway"}` {
		t.Fatalf("listener fetch: %d %s %v", response.StatusCode, raw, err)
	}

	// Режим пересчитывается на каждом запросе: origin без режима не отдаётся.
	setProjectRepository(t, f.ts, f.swarm, "")
	expectWorkspace(t, "cleared repository", f.serve("GET", clone, nil, bearer(f.token(f.clone))), "", "")
	other := "https://git.example.com/group/other.git"
	f.ts.srv.git.remotes[other] = gitCredential{username: "brood-bot", token: gitTokenSentinel}
	setProjectRepository(t, f.ts, f.swarm, other)
	expectWorkspace(t, "other repository", f.serve("GET", clone, nil, bearer(f.token(f.clone))), "", "")
	setProjectRepository(t, f.ts, f.swarm, gitTestOrigin)
	credential := f.ts.srv.git.remotes[gitTestOrigin]
	delete(f.ts.srv.git.remotes, gitTestOrigin)
	expectWorkspace(t, "removed remote", f.serve("GET", clone, nil, bearer(f.token(f.clone))), "", "")
	f.ts.srv.git.remotes[gitTestOrigin] = credential
	expectWorkspace(t, "restored", f.serve("GET", clone, nil, bearer(f.token(f.clone))), gitTestOrigin, "gateway")
	expectWorkspace(t, "direct without repository dependency", f.serve("GET", direct, nil, bearer(f.token(f.direct))), workspaceTestDirect, "direct")
}

func TestGuestWorkspaceOriginAuthorization(t *testing.T) {
	f := workspaceSetup(t)
	clone := workspacePath(f.clone)
	token := f.token(f.clone)
	next := f.clone.Incarnation + 1
	nextToken := guestToken(f.ts.srv.managed.registry.relaySecret, f.clone.UID, next)
	front, err := url.Parse(f.ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	cookie := http.Header{}
	for _, c := range f.alice.Jar.Cookies(front) {
		cookie.Add("Cookie", c.String())
	}
	if cookie.Get("Cookie") == "" {
		t.Fatal("operator session cookie missing")
	}
	withCookie := bearer("wrong")
	withCookie["Cookie"] = cookie["Cookie"]
	for _, tc := range []struct {
		name, path string
		header     http.Header
	}{
		{"no token", clone, nil},
		{"empty bearer", clone, http.Header{"Authorization": {"Bearer "}}},
		{"two tokens", clone, http.Header{"Authorization": {"Bearer " + token, "Bearer " + token}}},
		{"basic", clone, http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+token))}}},
		{"lowercase scheme", clone, http.Header{"Authorization": {"bearer " + token}}},
		{"wrong token", clone, bearer(strings.Repeat("A", 43))},
		{"truncated token", clone, bearer(token[:42])},
		{"other actor token", clone, bearer(f.token(f.direct))},
		{"other generation token", clone, bearer(nextToken)},
		{"future generation path", fmt.Sprintf("/api/proxmox/workspace/%s/%d", f.clone.UID, next), bearer(nextToken)},
		{"current token on future generation path", fmt.Sprintf("/api/proxmox/workspace/%s/%d", f.clone.UID, next), bearer(token)},
		{"other actor path", workspacePath(f.direct), bearer(token)},
		{"unknown actor signed token", "/api/proxmox/workspace/unknown-actor/1", bearer(guestToken(f.ts.srv.managed.registry.relaySecret, "unknown-actor", 1))},
		{"cookie only", clone, cookie},
		{"cookie with wrong token", clone, withCookie},
	} {
		expectWorkspaceDenied(t, tc.name, f.serve("GET", tc.path, nil, tc.header), http.StatusUnauthorized, f)
	}
	ok := bearer(token)
	for _, tc := range []struct {
		method, path string
		body         io.Reader
		want         int
	}{
		{"POST", clone, nil, 405},
		{"PUT", clone, strings.NewReader("{}"), 405},
		{"DELETE", clone, nil, 405},
		{"HEAD", clone, nil, 405},
		{"GET", clone + "?x=1", nil, 404},
		{"GET", clone + "?", nil, 404},
		{"GET", clone + "/", nil, 404},
		{"GET", clone + "/origin", nil, 404},
		{"GET", "/api/proxmox/workspace/" + f.clone.UID, nil, 404},
		{"GET", "/api/proxmox/workspace/", nil, 404},
		{"GET", fmt.Sprintf("/api/proxmox/workspace/%s/0%d", f.clone.UID, f.clone.Incarnation), nil, 404},
		{"GET", fmt.Sprintf("/api/proxmox/workspace/%s/0", f.clone.UID), nil, 404},
		{"GET", fmt.Sprintf("/api/proxmox/workspace/%s/-1", f.clone.UID), nil, 404},
		{"GET", fmt.Sprintf("/api/proxmox/workspace/%s/x", f.clone.UID), nil, 404},
		{"GET", fmt.Sprintf("/api/proxmox/workspace/bad.uid/%d", f.clone.Incarnation), nil, 404},
		{"GET", fmt.Sprintf("/api/proxmox/workspace/%s/%%3%d", f.clone.UID, f.clone.Incarnation), nil, 404},
		{"GET", clone, strings.NewReader("body"), 404},
	} {
		w := f.serve(tc.method, tc.path, tc.body, ok)
		expectWorkspaceDenied(t, tc.method+" "+tc.path, w, tc.want, f)
		if tc.want == 405 && w.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("%s: Allow=%q", tc.method, w.Header().Get("Allow"))
		}
	}

	ctx := context.Background()
	swarm, err := f.ts.srv.store.GetSwarm(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	f.ts.fake.mu.Lock()
	metadata := f.ts.fake.atespaces[swarm.AtespaceName].Metadata
	namespace := metadata.Uid
	metadata.Uid = "replacement-namespace"
	f.ts.fake.mu.Unlock()
	expectWorkspaceDenied(t, "changed namespace", f.serve("GET", clone, nil, ok), http.StatusServiceUnavailable, f)
	f.ts.fake.mu.Lock()
	metadata.Uid = namespace
	f.ts.fake.mu.Unlock()
	expectWorkspace(t, "restored namespace", f.serve("GET", clone, nil, ok), gitTestOrigin, "gateway")

	// Reset отзывает поколение с первого шага: прежний token и путь — 401.
	current, err := f.ts.srv.store.GetManagedActor(ctx, f.clone.UID)
	if err == nil {
		_, err = f.ts.srv.store.BeginManagedActorOperation(ctx, current.UID, current.Revision, store.NewID(), "reset")
	}
	if err != nil {
		t.Fatal(err)
	}
	expectWorkspaceDenied(t, "stale generation", f.serve("GET", clone, nil, ok), http.StatusUnauthorized, f)
	direct, err := f.ts.srv.store.GetManagedActor(ctx, f.direct.UID)
	if err == nil {
		direct, err = f.ts.srv.store.ConfirmManagedActorStage(ctx, direct.UID, direct.Revision, direct.OperationID, "start", "running")
	}
	if err == nil {
		_, err = f.ts.srv.store.BeginManagedActorOperation(ctx, direct.UID, direct.Revision, store.NewID(), "stop")
	}
	if err != nil {
		t.Fatal(err)
	}
	expectWorkspaceDenied(t, "stopped actor", f.serve("GET", workspacePath(f.direct), nil, bearer(f.token(f.direct))), http.StatusUnauthorized, f)
}

// SPEC: An actor without origin gets both fields empty, also after the project
// selects a repository: the origin is fixed at creation.
func TestGuestWorkspaceOriginEmpty(t *testing.T) {
	ts, alice, swarm, _, hub := managedOriginSetup(t, "")
	ctx := context.Background()
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), managedOriginBody("plain", nil))
	expectStatus(t, "create plain", r, b, http.StatusCreated)
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	for range 3 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	plain, err := ts.srv.store.LookupManagedActor(ctx, swarm, "plain")
	if err != nil || plain.Phase != "running" || plain.Origin != "" {
		t.Fatalf("actor without origin: %+v %v", plain, err)
	}
	f := workspaceFixture{ts: ts, alice: alice, swarm: swarm, clone: plain}
	expectWorkspace(t, "empty", f.serve("GET", workspacePath(plain), nil, bearer(f.token(plain))), "", "")
	setProjectRepository(t, ts, swarm, gitTestOrigin)
	expectWorkspace(t, "empty after repository", f.serve("GET", workspacePath(plain), nil, bearer(f.token(plain))), "", "")
	expectWorkspaceDenied(t, "empty actor cookie", f.serve("GET", workspacePath(plain), nil, nil), http.StatusUnauthorized, f)
}

// FEATURE: Actor Git identity delivered with the workspace origin (#249).
// SPEC: With BROOD_ACTOR_GIT_DOMAIN set, the workspace response of an actor
// with an origin carries gitIdentity {name: "brood <actor>", email:
// "<actor>@<domain>"}; without the setting, or for an actor without origin,
// the body is exactly {origin, mode} as before. Denied responses stay as they
// were and never carry the identity.
func TestGuestWorkspaceGitIdentity(t *testing.T) {
	f := workspaceSetup(t)
	clone := workspacePath(f.clone)
	expectWorkspace(t, "unset domain", f.serve("GET", clone, nil, bearer(f.token(f.clone))), gitTestOrigin, "gateway")

	f.ts.srv.actorGitDomain = "brood.corp.example"
	w := f.serve("GET", clone, nil, bearer(f.token(f.clone)))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("identity: status=%d header=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
	var got map[string]any
	want := map[string]any{"origin": gitTestOrigin, "mode": "gateway", "gitIdentity": map[string]any{"name": "brood clone", "email": "clone@brood.corp.example"}}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("identity body %s; want %v", w.Body.String(), want)
	}
	direct := f.serve("GET", workspacePath(f.direct), nil, bearer(f.token(f.direct)))
	if err := json.Unmarshal(direct.Body.Bytes(), &got); err != nil || got["gitIdentity"] == nil || got["mode"] != "direct" {
		t.Fatalf("direct identity body %s", direct.Body.String())
	}

	// Без origin нет клона, куда писать идентичность: тело прежнее.
	setProjectRepository(t, f.ts, f.swarm, "")
	expectWorkspace(t, "no origin with domain", f.serve("GET", clone, nil, bearer(f.token(f.clone))), "", "")
	setProjectRepository(t, f.ts, f.swarm, gitTestOrigin)
	expectWorkspaceDenied(t, "wrong token with domain", f.serve("GET", clone, nil, bearer(strings.Repeat("A", 43))), http.StatusUnauthorized, f)
}

// FEATURE: Installation Claude Code models delivered with the workspace (#263).
// SPEC: With BROOD_ACTOR_MODEL or BROOD_ACTOR_SMALL_MODEL set, the workspace
// response carries models {model, smallModel} with only the set names, for an
// actor with or without origin: Claude runs in every actor, a clone is not
// needed. Without both settings the body is exactly {origin, mode} as before.
// Denied responses never carry the models.
func TestGuestWorkspaceActorModels(t *testing.T) {
	f := workspaceSetup(t)
	clone := workspacePath(f.clone)
	expectWorkspace(t, "unset models", f.serve("GET", clone, nil, bearer(f.token(f.clone))), gitTestOrigin, "gateway")

	body := func(what string) map[string]any {
		t.Helper()
		w := f.serve("GET", clone, nil, bearer(f.token(f.clone)))
		var got map[string]any
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("%s: status=%d header=%v body=%s", what, w.Code, w.Header(), w.Body.String())
		}
		return got
	}
	f.ts.srv.actorModels = actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"}
	both := map[string]any{"model": "glm-5.3", "smallModel": "glm-5.3-flash"}
	if got, want := body("both"), map[string]any{"origin": gitTestOrigin, "mode": "gateway", "models": both}; !reflect.DeepEqual(got, want) {
		t.Fatalf("both models: %v; want %v", got, want)
	}
	f.ts.srv.actorModels = actormodel.Models{Small: "glm-5.3-flash"}
	if got, want := body("small"), map[string]any{"origin": gitTestOrigin, "mode": "gateway", "models": map[string]any{"smallModel": "glm-5.3-flash"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("small model: %v; want %v", got, want)
	}

	// Без origin клона нет, но Claude запускается: модели идут и так.
	f.ts.srv.actorModels = actormodel.Models{Model: "glm-5.3", Small: "glm-5.3-flash"}
	setProjectRepository(t, f.ts, f.swarm, "")
	if got, want := body("no origin"), map[string]any{"origin": "", "mode": "", "models": both}; !reflect.DeepEqual(got, want) {
		t.Fatalf("models without origin: %v; want %v", got, want)
	}
	setProjectRepository(t, f.ts, f.swarm, gitTestOrigin)
	denied := f.serve("GET", clone, nil, bearer(strings.Repeat("A", 43)))
	expectWorkspaceDenied(t, "wrong token with models", denied, http.StatusUnauthorized, f)
	if strings.Contains(denied.Body.String(), "glm-5.3") {
		t.Fatalf("denied response carried the models: %s", denied.Body.String())
	}
}
