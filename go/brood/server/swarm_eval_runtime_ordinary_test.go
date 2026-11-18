package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// FEATURE: Eval isolation is opt-in by retained trusted marker.
// SPEC: Ordinary guest exports, legacy credentials, Git push and model choice
// stay unchanged; a missing eval row is not a refusal.
func TestEvalRuntimeOrdinaryUnchanged(t *testing.T) {
	f := gitGatewaySetup(t) // Installation credential only, no eval marker.
	for _, operation := range []string{"git-upload-pack", "git-receive-pack"} {
		path := f.base() + "/info/refs?service=" + operation
		if code, body := f.guestGet(t, path); code != 200 {
			t.Fatalf("ordinary discovery %s: %d %s", operation, code, body)
		}
		h := bearer(f.token)
		h.Set("Content-Type", "application/x-"+operation+"-request")
		if w := f.serve("POST", f.base()+"/"+operation, strings.NewReader("packet"), h); w.Code != 200 {
			t.Fatalf("ordinary Git %s: %d %s", operation, w.Code, w.Body)
		}
	}
	for _, path := range []string{workspacePath(f.actor), fmt.Sprintf("/api/proxmox/secrets/%s/%d", f.actor.UID, f.actor.Incarnation)} {
		if code, body := f.guestGet(t, path); code != 200 {
			t.Fatalf("ordinary guest delivery: %d %s", code, body)
		}
	}
	// Removing a project credential still permits the ordinary legacy fallback.
	putProjectGitCredential(t, f.ts, f.project, "temporary", gitTestOrigin, gitTokenSentinel)
	ctx := context.Background()
	if err := f.ts.srv.store.SetProjectGitCredentialRef(ctx, f.project, "temporary"); err != nil {
		t.Fatal(err)
	}
	if err := f.ts.srv.store.RemoveProjectGitCredentials(ctx, f.project, []string{"temporary"}); err != nil {
		t.Fatal(err)
	}
	if code, body := f.guestGet(t, f.base()+"/info/refs?service=git-upload-pack"); code != 200 {
		t.Fatalf("ordinary legacy fallback: %d %s", code, body)
	}
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":"ordinary-backend"}`))
	}))
	defer backend.Close()
	ts, _, _, base, token := managedLLMSetup(t, backend.URL, "ordinary-server-key")
	for _, operation := range []string{"messages", "messages/count_tokens"} {
		if w := llmRequest(t, ts.srv.Handler(), "POST", base+"/v1/"+operation, token, strings.NewReader(`{"model":"ordinary-choice"}`)); w.Code != 200 {
			t.Fatalf("ordinary model choice: %d %s", w.Code, w.Body)
		}
	}
	if w := llmRequest(t, ts.srv.Handler(), "GET", base+"/v1/models", token, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "ordinary-backend") {
		t.Fatalf("ordinary model catalog: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 3 {
		t.Fatalf("ordinary upstream calls: %d", calls.Load())
	}
}
