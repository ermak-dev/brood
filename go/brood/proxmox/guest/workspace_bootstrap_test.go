//go:build linux

package guest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Явно согласованный receiver эфемерного bootstrap без изменения legacy (#292).
// SPEC: Новый header только opt-in, narrow commit без payload/authority,
// строгий отказ unknown/duplicate/null; legacy не рекламирует поддержку.
// IMPLEMENTATION: Opt-in fetch и общий строгий decoder ответа и root-проекции.
// RESULT: IMPLEMENTED
// STATUS: DONE
func workspaceBootstrapFixture(t *testing.T, body, protocol string) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/proxmox/workspace/actor/1" || r.Header.Get("Authorization") != "Bearer "+gitTestToken || r.Header.Get("Cookie") != "" {
			t.Error("bootstrap receiver changed scoped request or credential isolation")
		}
		if r.Header.Get("X-Brood-Workspace-Protocol") != protocol {
			t.Error("bootstrap capability was not explicit or legacy advertised it")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestOptInWorkspaceBootstrapReadsOnlyPinnedCommit(t *testing.T) {
	pin := strings.Repeat("a", 40)
	body := fmt.Sprintf(`{"origin":%q,"mode":"gateway","ephemeral":{"commit":%q}}`, cloneTestOrigin, pin)
	server := workspaceBootstrapFixture(t, body, "ephemeral-v1")
	got, err := FetchWorkspaceBootstrap(context.Background(), gitTestConfig(server), nil)
	if err != nil || got.Origin != cloneTestOrigin || got.Mode != "gateway" || got.Ephemeral == nil || got.Ephemeral.Commit != pin {
		t.Fatalf("pinned bootstrap unavailable: %+v %v", got, err)
	}
	run := privateTemp(t)
	if err := publishWorkspaceOrigin(run, got, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	projected, err := readWorkspaceOrigin(run, os.Geteuid(), os.Getegid())
	if err != nil || !reflect.DeepEqual(projected, got) {
		t.Fatalf("root-owned projection lost bootstrap: %+v %v", projected, err)
	}
}

func TestLegacyWorkspaceReceiverNeitherAdvertisesNorAcceptsEphemeral(t *testing.T) {
	body := fmt.Sprintf(`{"origin":%q,"mode":"gateway","ephemeral":{"commit":%q}}`, cloneTestOrigin, strings.Repeat("a", 40))
	server := workspaceBootstrapFixture(t, body, "")
	got, err := FetchWorkspaceOrigin(context.Background(), gitTestConfig(server), nil)
	if err == nil || got != (WorkspaceOrigin{}) {
		t.Fatalf("legacy silently accepted unsupported bootstrap: %+v %v", got, err)
	}
}

func TestOptInWorkspaceBootstrapStillAcceptsLegacyPersistentResponse(t *testing.T) {
	server := workspaceBootstrapFixture(t, fmt.Sprintf(`{"origin":%q,"mode":"gateway"}`, cloneTestOrigin), "ephemeral-v1")
	got, err := FetchWorkspaceBootstrap(context.Background(), gitTestConfig(server), nil)
	if err != nil || got.Origin != cloneTestOrigin || got.Ephemeral != nil {
		t.Fatalf("legacy persistent response changed: %+v %v", got, err)
	}
}

func TestOptInWorkspaceBootstrapRefusesMalformedOrBroaderScope(t *testing.T) {
	pin := strings.Repeat("a", 40)
	for _, object := range []string{
		`null`, `{}`, `{"commit":"HEAD"}`, `{"commit":null}`,
		fmt.Sprintf(`{"commit":%q}`, strings.Repeat("A", 40)),
		fmt.Sprintf(`{"commit":%q,"commit":%q}`, pin, pin),
		fmt.Sprintf(`{"commit":%q,"payload":"private task"}`, pin),
		fmt.Sprintf(`{"commit":%q,"lease":"private root state"}`, pin),
		fmt.Sprintf(`{"commit":%q,"class":"persistent"}`, pin),
	} {
		t.Run(object, func(t *testing.T) {
			body := fmt.Sprintf(`{"origin":%q,"mode":"gateway","ephemeral":%s}`, cloneTestOrigin, object)
			server := workspaceBootstrapFixture(t, body, "ephemeral-v1")
			got, err := FetchWorkspaceBootstrap(context.Background(), gitTestConfig(server), nil)
			if err == nil || got != (WorkspaceOrigin{}) {
				t.Fatalf("untrusted bootstrap accepted: %+v %v", got, err)
			}
		})
	}
	for _, body := range []string{
		fmt.Sprintf(`{"origin":"","mode":"","ephemeral":{"commit":%q}}`, pin),
		fmt.Sprintf(`{"origin":%q,"mode":"gateway","ephemeral":{"commit":%q},"ephemeral":{"commit":%q}}`, cloneTestOrigin, pin, pin),
	} {
		server := workspaceBootstrapFixture(t, body, "ephemeral-v1")
		got, err := FetchWorkspaceBootstrap(context.Background(), gitTestConfig(server), nil)
		if err == nil || got != (WorkspaceOrigin{}) {
			t.Fatalf("ambiguous bootstrap accepted: %+v %v", got, err)
		}
	}
}

func TestWorkspaceProjectionRefusesEphemeralWithoutCanonicalPinAndOrigin(t *testing.T) {
	for _, origin := range []WorkspaceOrigin{
		{Ephemeral: &EphemeralWorkspace{Commit: strings.Repeat("a", 40)}},
		{Origin: cloneTestOrigin, Mode: "gateway", Ephemeral: &EphemeralWorkspace{}},
		{Origin: cloneTestOrigin, Mode: "gateway", Ephemeral: &EphemeralWorkspace{Commit: "HEAD"}},
	} {
		run := privateTemp(t)
		if err := publishWorkspaceOrigin(run, origin, os.Geteuid(), os.Getegid()); err == nil {
			t.Fatal("invalid ephemeral projection published")
		}
		if len(entries(t, run)) != 0 {
			t.Fatal("invalid bootstrap projection left files")
		}
	}
}
