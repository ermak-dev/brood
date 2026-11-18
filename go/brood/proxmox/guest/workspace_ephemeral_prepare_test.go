//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: Фиксированный UID1000 helper готовит только согласованный root bootstrap (#292).
// SPEC: Путь не приходит из запроса, pin/origin/mode/identity совпадают с root-owned
// проекцией; реальный Git clone завершается до возврата, direct relay-вызов закрыт.
// IMPLEMENTATION: Root-internal операция существующего фиксированного session helper.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestEphemeralPreparationClonesOnlyRootPinnedSource(t *testing.T) {
	remote := newGitRemoteFixture(t)
	pin := cloneTestGit(t, remote.bare, "rev-parse", "HEAD")
	newer := cloneTestGit(t, remote.bare, "commit-tree", "HEAD^{tree}", "-p", pin, "-m", "advance default")
	cloneTestGit(t, remote.bare, "update-ref", "refs/heads/main", newer, pin)
	run, project, home := privateTemp(t), privateTemp(t), privateTemp(t)
	origin := WorkspaceOrigin{Origin: cloneTestOrigin, Mode: "gateway", Ephemeral: &EphemeralWorkspace{Commit: pin}}
	if err := publishWorkspaceOrigin(run, origin, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	layout := appLayout{Project: project, Home: home, Run: run, RunUID: os.Geteuid(), RunGID: os.Getegid(), Gateway: remote.gateway, CloneTimeout: 30 * time.Second}
	request := sessionctl.CheckoutRequest{Origin: cloneTestOrigin, Mode: "gateway", Commit: pin}
	if err := prepareEphemeralWorkspace(context.Background(), layout, request); err != nil {
		t.Fatal(err)
	}
	if got := cloneTestGit(t, project, "rev-parse", "HEAD"); got != pin {
		t.Fatalf("bootstrap helper cloned default HEAD: %s, want %s", got, pin)
	}
	if got := cloneTestGit(t, project, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatal("bootstrap helper left a mutable branch checkout")
	}
	if _, err := os.Stat(filepath.Join(project, "README")); err != nil {
		t.Fatal(err)
	}
	if err := prepareEphemeralWorkspace(context.Background(), layout, request); err == nil {
		t.Fatal("bootstrap helper silently reused nonempty workspace")
	}
}

func TestEphemeralPreparationRefusesForeignProjectionBeforeNetwork(t *testing.T) {
	for _, scenario := range []string{"legacy", "commit", "origin", "mode", "identity"} {
		t.Run(scenario, func(t *testing.T) {
			remote := newGitRemoteFixture(t)
			pin := cloneTestGit(t, remote.bare, "rev-parse", "HEAD")
			run, project, home := privateTemp(t), privateTemp(t), privateTemp(t)
			origin := WorkspaceOrigin{Origin: cloneTestOrigin, Mode: "gateway", Ephemeral: &EphemeralWorkspace{Commit: pin}}
			if scenario == "legacy" {
				origin.Ephemeral = nil
			}
			if err := publishWorkspaceOrigin(run, origin, os.Geteuid(), os.Getegid()); err != nil {
				t.Fatal(err)
			}
			request := sessionctl.CheckoutRequest{Origin: cloneTestOrigin, Mode: "gateway", Commit: pin}
			switch scenario {
			case "commit":
				request.Commit = strings.Repeat("c", 40)
			case "origin":
				request.Origin = "https://git.example.org/other/repo.git"
			case "mode":
				request.Mode = "direct"
			case "identity":
				request.GitIdentity = attestIdentity
			}
			layout := appLayout{Project: project, Home: home, Run: run, RunUID: os.Geteuid(), RunGID: os.Getegid(), Gateway: remote.gateway, CloneTimeout: 30 * time.Second}
			if err := prepareEphemeralWorkspace(context.Background(), layout, request); err == nil {
				t.Fatal("foreign bootstrap selection was accepted")
			}
			if remote.requests.Load() != 0 || len(entries(t, project)) != 0 {
				t.Fatal("foreign selection reached network or workspace")
			}
		})
	}
}

func TestEphemeralPreparationOperationCannotBeCalledThroughRelay(t *testing.T) {
	connection, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	handler := newSessionHandler(connection, nil, nil, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		called = true
		return relay.SessionResponse{OK: true}
	})
	request := relay.SessionRequest{Operation: ephemeralPreparationOperation, Arguments: json.RawMessage(`{}`), Caller: "server"}
	for _, response := range []relay.SessionResponse{handler(context.Background(), request), HandleSession(context.Background(), request)} {
		if response.OK || !strings.Contains(response.Text, "internal") {
			t.Fatalf("bootstrap operation reached external dispatcher: %+v", response)
		}
	}
	if called {
		t.Fatal("relay reached root-internal preparation")
	}
}

func TestEphemeralPreparationRequestFitsExistingFixedHelperProtocol(t *testing.T) {
	args, err := json.Marshal(sessionctl.CheckoutRequest{Origin: cloneTestOrigin, Mode: "gateway", Commit: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(relay.SessionRequest{Operation: ephemeralPreparationOperation, Arguments: args, Caller: "supervisor"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := ReadSessionRequest(bytes.NewReader(data))
	if err != nil || request.Operation != ephemeralPreparationOperation || request.Caller != "supervisor" {
		t.Fatalf("fixed helper protocol rejected internal preparation: %+v %v", request, err)
	}
	if _, err := decodeEphemeralPreparation(request.Arguments); err != nil {
		t.Fatal(err)
	}
}

func TestEphemeralPreparationRequestRejectsActorSelectedPaths(t *testing.T) {
	pin := strings.Repeat("a", 40)
	for _, raw := range []string{
		`null`, `{}`,
		fmt.Sprintf(`{"commit":%q,"origin":%q,"mode":"gateway","project":"/chosen"}`, pin, cloneTestOrigin),
		fmt.Sprintf(`{"commit":%q,"origin":%q,"mode":"gateway","command":"chosen"}`, pin, cloneTestOrigin),
		fmt.Sprintf(`{"commit":"HEAD","origin":%q,"mode":"gateway"}`, cloneTestOrigin),
	} {
		if _, err := decodeEphemeralPreparation(json.RawMessage(raw)); err == nil {
			t.Fatalf("untrusted preparation request accepted: %s", raw)
		}
	}
}
