//go:build linux

package main

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: Supervisor явно рекламирует готовый ephemeral bootstrap и публикует только валидный DTO.
// SPEC: Новый receiver с capability header, без cache после invalid response;
// отказ публикации не возвращает частично принятый bootstrap.
// IMPLEMENTATION: Supervisor связывает opt-in fetch с root publisher.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSupervisorBootstrapOriginNegotiatesBeforeProjection(t *testing.T) {
	for _, scenario := range []string{"success", "malformed", "publish-failure"} {
		t.Run(scenario, func(t *testing.T) {
			pin := strings.Repeat("a", 40)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/proxmox/workspace/actor/1" || r.Header.Get("X-Brood-Workspace-Protocol") != "ephemeral-v1" || r.Header.Get("Cookie") != "" {
					t.Error("supervisor used legacy or unscoped bootstrap receiver")
				}
				w.Header().Set("Content-Type", "application/json")
				if scenario == "malformed" {
					fmt.Fprint(w, `{"origin":"https://git.example.org/group/repo.git","mode":"gateway","ephemeral":null}`)
				} else {
					fmt.Fprintf(w, `{"origin":"https://git.example.org/group/repo.git","mode":"gateway","ephemeral":{"commit":%q}}`, pin)
				}
			}))
			defer server.Close()
			config := relay.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https") + "/api/proxmox/guest/actor/1", ActorUID: "actor", Generation: 1, Token: strings.Repeat("t", 43), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
			var published guest.WorkspaceOrigin
			calls := 0
			got, err := fetchAndPublishWorkspaceBootstrap(context.Background(), config, func(origin guest.WorkspaceOrigin) error {
				calls++
				published = origin
				if scenario == "publish-failure" {
					return errors.New("fixture publication refused")
				}
				return nil
			})
			if scenario == "success" {
				if err != nil || calls != 1 || got.Ephemeral == nil || got.Ephemeral.Commit != pin || !reflect.DeepEqual(got, published) {
					t.Fatalf("negotiated bootstrap not published: %+v %v", got, err)
				}
			} else {
				want := 1
				if scenario == "malformed" {
					want = 0
				}
				if err == nil || got != (guest.WorkspaceOrigin{}) || calls != want {
					t.Fatalf("invalid/failed bootstrap published or returned: %+v calls=%d err=%v", got, calls, err)
				}
			}
		})
	}
}
