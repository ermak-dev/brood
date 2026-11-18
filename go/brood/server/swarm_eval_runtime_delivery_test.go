package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/proxmox/egress"
	"brood.dev/go/brood/store"
)

// FEATURE: Eval workspace admission and ongoing Git isolation.
// SPEC: No public direct origin, no missing policy/credential; boot models are
// exactly pinned. Active Git streams recheck retained policy every second.
func TestEvalRuntimeOriginAdmissionAndPinnedBoot(t *testing.T) {
	for _, tc := range []struct {
		name, origin        string
		credential, network bool
		want                int
	}{
		{"legacy", gitTestOrigin, false, true, 422},
		{"no-policy", gitTestOrigin, true, false, 422},
		{"direct", "https://github.com/owner/public", true, true, 422},
		{"gateway", gitTestOrigin, true, true, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, alice, swarm, pve, _ := managedOriginSetup(t, gitTestOrigin)
			ctx := context.Background()
			sw, err := ts.srv.store.GetSwarm(ctx, swarm)
			if err != nil {
				t.Fatal(err)
			}
			if tc.credential {
				putProjectGitCredential(t, ts, sw.ProjectID, "candidate", gitTestOrigin, gitTokenSentinel)
				if err := ts.srv.store.SetProjectGitCredentialRef(ctx, sw.ProjectID, "candidate"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.network {
				policy, err := egress.Compile([]egress.Rule{{Protocol: egress.TLS, Hostnames: []string{"github.com"}, Ports: []int{443}}})
				if err != nil {
					t.Fatal(err)
				}
				raw, err := policy.Encode()
				if err != nil {
					t.Fatal(err)
				}
				wire := string(raw)
				root, err := ts.srv.store.GetUserByUsername(ctx, "root")
				if err != nil {
					t.Fatal(err)
				}
				_, err = ts.srv.store.SyncProject(ctx, root.ID, sw.ProjectID, store.ProjectDeclaration{Name: "proj", Repository: gitTestOrigin, ManagedNetwork: &wire}, func(context.Context) bool { return true })
				if err != nil {
					t.Fatal(err)
				}
			}
			runtimeEvalMarker(t, ts, swarm)
			ts.srv.actorModels = actormodel.Models{Model: "installation-model", Small: "installation-small"}
			r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), managedOriginBody("candidate", tc.origin))
			expectStatus(t, "eval admission", r, b, tc.want)
			pve.mu.Lock()
			creates := pve.creates
			pve.mu.Unlock()
			if tc.want != 201 {
				if creates != 0 {
					t.Fatal("refused eval creation reached provider")
				}
				actors, err := ts.srv.store.ListManagedActors(ctx, swarm)
				if err != nil || len(actors) != 0 {
					t.Fatalf("refused eval reservation persisted: %v %v", actors, err)
				}
				return
			}
			actor := workspaceStartIntent(t, ts, swarm, "candidate")
			token := guestToken(ts.srv.managed.registry.relaySecret, actor.UID, actor.Incarnation)
			w := llmRequest(t, ts.srv.Handler(), "GET", workspacePath(actor), token, nil)
			var body workspaceOrigin
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.Mode != "gateway" || body.Origin != gitTestOrigin || body.Models != (actormodel.Models{Model: "selected-model-1", Small: "selected-model-1"}) {
				t.Fatalf("eval boot: %d %s %v", w.Code, w.Body, err)
			}
			if strings.Contains(w.Body.String(), gitTokenSentinel) {
				t.Fatal("eval boot leaked Git credential")
			}
		})
	}
}

func TestEvalRuntimeGitOngoingAuthority(t *testing.T) {
	for _, operation := range []string{"info/refs?service=git-receive-pack", "git-receive-pack", "info/refs?service=git-upload-pack"} {
		t.Run(operation, func(t *testing.T) {
			f := projectCredentialGateway(t)
			canceled := make(chan struct{}, 1)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			f.upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "first")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					canceled <- struct{}{}
				case <-release:
				}
			}))
			method := "GET"
			var body io.Reader
			if operation == "git-receive-pack" {
				method = "POST"
				body = strings.NewReader("push")
			}
			req, err := http.NewRequest(method, f.ts.http.URL+f.base()+"/"+operation, body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+f.token)
			req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			first := make([]byte, 5)
			if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first" {
				t.Fatalf("stream start: %q %v", first, err)
			}
			if strings.Contains(operation, "receive-pack") {
				// Adversarial marker injection tests the watcher even though real binding
				// must reject existing actors. It is not a supported production bind path.
				runtimeEvalMarker(t, f.ts, f.swarm)
			} else {
				runtimeEvalMarker(t, f.ts, f.swarm)
				if err := f.ts.srv.store.RemoveProjectGitCredentials(context.Background(), f.project, []string{"gitlab"}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-canceled:
			case <-time.After(4 * time.Second):
				t.Fatal("eval restriction did not cancel active upstream")
			}
			_, _ = io.Copy(io.Discard, response.Body)
			if got := len(f.upstream.seen()); got != 1 {
				t.Fatalf("unexpected extra upstream requests: %d", got)
			}
		})
	}
}

func TestEvalRuntimeModelReadError(t *testing.T) {
	ts, _, _, base, token := managedLLMSetup(t, "http://127.0.0.1:1", "unused-key")
	if _, err := ts.srv.catalog.db.Exec(`DROP TABLE swarm_eval_bindings`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/models", "/v1/messages/count_tokens"} {
		method := "POST"
		if path == "/v1/models" {
			method = "GET"
		}
		w := llmRequest(t, ts.srv.Handler(), method, base+path, token, strings.NewReader(`{"model":"selected-model-1"}`))
		if w.Code != 503 {
			t.Fatalf("model marker error: %d %s", w.Code, w.Body)
		}
		w = llmRequest(t, ts.srv.Handler(), method, base+path, strings.Repeat("x", 43), nil)
		if w.Code != 401 {
			t.Fatal(fmt.Sprintf("model marker read preceded auth: %d", w.Code))
		}
	}
}
