package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// FEATURE: Trusted eval markers isolate guest effects.
// SPEC: Auth precedes marker reads; errors fail closed. Eval secrets and pushes
// never reach their providers, fetch needs a candidate credential, workspace
// requires explicit policy and gateway origin, and model selection is pinned.
// IMPLEMENTATION: Retained-marker guards precede guest/provider effects; model
// validation runs before the reverse proxy, and native Apply repeats policy reads.
// RESULT: Focused normal/race tests pass; full-suite verification is blocked by
// the independent legacy-51 fixture retaining the supporting schema-59 tables.
// These adversarial fixtures insert retained markers directly after actor setup;
// they test runtime guards, not Store's separate bind-before-inventory admission.
func runtimeEvalMarker(t *testing.T, ts *substrateServer, swarm string) {
	t.Helper()
	sw, err := ts.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil {
		t.Fatal(err)
	}
	b := store.SwarmEvalBinding{SwarmEvalBindingInput: store.SwarmEvalBindingInput{CandidateSwarmID: swarm, Model: "selected-model-1"}, CandidateProjectID: sw.ProjectID, NonProduction: true}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ts.srv.catalog.db.Exec(`INSERT INTO swarm_eval_bindings(candidate_swarm_id,binding,nonproduction) VALUES(?,?,1)`, swarm, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestEvalRuntimeGitAndSecrets(t *testing.T) {
	f := projectCredentialGateway(t)
	runtimeEvalMarker(t, f.ts, f.swarm)
	refs := f.base() + "/info/refs?service=git-upload-pack"
	if code, body := f.guestGet(t, refs); code != 200 {
		t.Fatalf("dedicated eval fetch: %d %s", code, body)
	}
	seen := len(f.upstream.seen())
	for _, path := range []string{f.base() + "/info/refs?service=git-receive-pack", f.base() + "/git-receive-pack"} {
		method := "GET"
		if strings.HasSuffix(path, "/git-receive-pack") {
			method = "POST"
		}
		h := bearer(f.token)
		h.Set("Content-Type", "application/x-git-receive-pack-request")
		var body io.Reader
		if method == "POST" {
			body = strings.NewReader("push")
		}
		w := f.serve(method, path, body, h)
		if w.Code != 403 {
			t.Errorf("eval push: %d %s", w.Code, w.Body)
		}
	}
	path := fmt.Sprintf("/api/proxmox/secrets/%s/%d", f.actor.UID, f.actor.Incarnation)
	// A denied export must not read even an unreadable project bundle.
	if _, err := f.ts.srv.catalog.db.Exec(`DROP TABLE project_secret_vars`); err != nil {
		t.Fatal(err)
	}
	code, body := f.guestGet(t, path)
	var projection struct {
		Revision int64             `json:"revision"`
		Files    []json.RawMessage `json:"files"`
		Vars     map[string]string `json:"vars"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &projection) != nil || projection.Revision != 0 || len(projection.Files) != 0 || len(projection.Vars) != 0 {
		t.Errorf("eval secret-free bootstrap: %d %s", code, body)
	}
	f.ts.srv.git.remotes[gitTestOrigin] = gitCredential{username: "installation", token: "must-not-use"}
	if err := f.ts.srv.store.RemoveProjectGitCredentials(context.Background(), f.project, []string{"gitlab"}); err != nil {
		t.Fatal(err)
	}
	if code, body := f.guestGet(t, refs); code != 403 || !strings.Contains(body, "candidate project Git credential") {
		t.Errorf("eval credential deletion: %d %s", code, body)
	}
	if len(f.upstream.seen()) != seen {
		t.Errorf("prohibited eval requests reached upstream: %d -> %d", seen, len(f.upstream.seen()))
	}
}

func TestEvalRuntimeWorkspace(t *testing.T) {
	f := workspaceSetup(t)
	runtimeEvalMarker(t, f.ts, f.swarm)
	f.ts.srv.actorModels = actormodel.Models{Model: "installation-model", Small: "installation-small"}
	for _, a := range []store.ManagedActor{f.clone, f.direct} {
		w := f.serve("GET", workspacePath(a), nil, bearer(f.token(a)))
		if w.Code != 403 {
			t.Errorf("eval workspace without explicit network/dedicated access: %d %s", w.Code, w.Body)
		}
	}
}

func TestEvalRuntimeModels(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "selected-model-1" || r.Header.Get("Authorization") != "Bearer server-only-key" {
			t.Errorf("eval upstream received wrong model or credential: model=%q error=%v", body.Model, err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	ts, _, actorPath, base, token := managedLLMSetup(t, backend.URL, "server-only-key")
	swarm := strings.Split(actorPath, "/")[3]
	runtimeEvalMarker(t, ts, swarm)
	h := ts.srv.Handler()
	for _, operation := range []string{"messages", "messages/count_tokens"} {
		for _, body := range []string{`{"model":"other"}`, `{}`, `{"model":null}`, `{"model":"selected-model-1","model":"other"}`, `{"Model":"selected-model-1"}`, `{"model":"selected-model-1"} {}`, `{"model":"selected-model-1"`} {
			w := llmRequest(t, h, "POST", base+"/v1/"+operation, token, strings.NewReader(body))
			if w.Code != 403 {
				t.Errorf("other/ambiguous eval model %s: %s: %d %s", operation, body, w.Code, w.Body)
			}
		}
	}
	w := llmRequest(t, h, "GET", base+"/v1/models", token, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"selected-model-1"`) {
		t.Errorf("pinned models: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 0 {
		t.Errorf("denied requests/model discovery made %d upstream calls", calls.Load())
	}
	for i, operation := range []string{"messages", "messages/count_tokens"} {
		w = llmRequest(t, h, "POST", base+"/v1/"+operation, token, strings.NewReader(`{"model":"selected-model-1"}`))
		if w.Code != 200 || calls.Load() != int32(i+1) {
			t.Errorf("selected model %s: %d %s calls=%d", operation, w.Code, w.Body, calls.Load())
		}
	}
}

func TestEvalRuntimeActorEffects(t *testing.T) {
	ts, alice, master, pve := masterSetup(t)
	worker := lifecycleWorker(t, ts, master, pve)
	pve.mu.Lock()
	beforeEffects := [4]int{pve.creates, pve.starts, pve.stops, pve.deletes}
	pve.mu.Unlock()
	runtimeEvalMarker(t, ts, master.SwarmID)
	ctx := context.Background()
	for _, action := range []string{"stop", "reset", "delete", "update"} {
		before, err := ts.srv.store.GetManagedActor(ctx, worker.UID)
		if err != nil {
			t.Fatal(err)
		}
		args := fmt.Sprintf(`{"name":%q}`, worker.Name)
		if action == "reset" || action == "delete" {
			args = fmt.Sprintf(`{"name":%q,"confirm":true}`, worker.Name)
		}
		_, err = ts.srv.managed.executeLifecycleCommand(ctx, master, lifecycleInvocation(action, args))
		if err == nil || !strings.Contains(err.Error(), "eval") {
			t.Errorf("eval actor %s: %v", action, err)
		}
		response := guestCommand(ts, master, action, args)
		if response.OK || !strings.Contains(response.Text, "eval") {
			t.Errorf("guest routing bypassed eval %s: %+v", action, response)
		}
		after, e := ts.srv.store.GetManagedActor(ctx, worker.UID)
		if e != nil || after.Revision != before.Revision {
			t.Fatalf("eval %s changed target: %v", action, e)
		}
	}
	_, err := ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation("landing", map[string]any{"action": "take", "request_id": "eval-take"}))
	if err == nil || !strings.Contains(err.Error(), "eval") {
		t.Errorf("eval landing take: %v", err)
	}
	sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if _, err := ts.srv.store.ShowLandingToken(ctx, sw.ProjectID); err != store.ErrNotFound {
		t.Errorf("eval acquired landing token: %v", err)
	}
	pve.mu.Lock()
	afterEffects := [4]int{pve.creates, pve.starts, pve.stops, pve.deletes}
	pve.mu.Unlock()
	if afterEffects != beforeEffects {
		t.Fatalf("eval actor commands reached provider: %v -> %v", beforeEffects, afterEffects)
	}
	// The separately authenticated panel/API owner path remains unrestricted.
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+master.SwarmID+"/actors/"+worker.Name, ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "eval owner panel cleanup", r, b, http.StatusOK)
	stopped := settleActor(t, ts, worker.UID, actorStopped)
	if stopped.Desired != "stopped" {
		t.Fatal("owner cleanup did not stop the eval actor")
	}
}

func TestEvalRuntimeNativeAdmission(t *testing.T) {
	ts, fake, _, alice, swarm, _ := quotaHTTPFixture(t)
	sw, err := ts.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil {
		t.Fatal(err)
	}
	runtimeEvalMarker(t, ts, swarm)
	req := httptest.NewRequest("GET", ts.http.URL+"/api/test", nil)
	for _, cookie := range alice.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	ran := false
	ts.srv.auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		for _, action := range []string{"create-actor", "update-actor", "resume-actor"} {
			native := &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Name: "denied", Atespace: sw.AtespaceName}, ActorTemplate: &ateapi.ObjectRef{Name: "base"}}
			o := &nativeWorkloadOperation{server: ts.srv, context: r.Context(), swarm: toAppSwarm(sw), action: action, name: "denied", actor: native, currentActor: native}
			if _, err := o.Prepare([]app.Swarm{toAppSwarm(sw)}); err == nil || !strings.Contains(err.Error(), "eval") {
				t.Errorf("native %s Prepare: %v", action, err)
			}
			// Apply is intentionally invoked independently: it may never trust Prepare.
			if err := o.Apply(nil); err == nil || !strings.Contains(err.Error(), "eval") {
				t.Errorf("native %s Apply: %v", action, err)
			}
		}
	})).ServeHTTP(httptest.NewRecorder(), req)
	if !ran {
		t.Fatal("authenticated admission fixture did not run")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.actors) != 0 {
		t.Fatal("eval mutated native inventory")
	}
}

func TestEvalRuntimeMarkerReadError(t *testing.T) {
	f := projectCredentialGateway(t)
	if _, err := f.ts.srv.catalog.db.Exec(`DROP TABLE swarm_eval_bindings`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.base() + "/info/refs?service=git-upload-pack", fmt.Sprintf("/api/proxmox/secrets/%s/%d", f.actor.UID, f.actor.Incarnation), workspacePath(f.actor)} {
		if code, body := f.guestGet(t, path); code != 503 {
			t.Errorf("marker read error: %d %s", code, body)
		}
	}
	if len(f.upstream.seen()) != 0 {
		t.Fatal("marker read error reached Git upstream")
	}
	if w := f.serve("GET", workspacePath(f.actor), nil, bearer(strings.Repeat("x", 43))); w.Code != 401 {
		t.Errorf("auth must precede marker: %d", w.Code)
	}
}
