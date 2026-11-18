package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: Root source API отдаёт только pin текущего allocated ephemeral actor (#292).
// SPEC: Negotiated guest обязателен; source до proof не раскрывает payload;
// текущие parent/owner/pool/binding проверяются, persistent wire не меняется.
// IMPLEMENTATION: Narrow DTO и два локальных scope snapshot вокруг ответа.
// RESULT: IMPLEMENTED
// STATUS: DONE
func ephemeralBootstrapSourceFixture(t *testing.T) (*dispatchServer, store.EphemeralChildIntent, store.ManagedActor) {
	t.Helper()
	d, intent, allocation := ephemeralAttestationServerFixture(t)
	d.srv.git.remotes[dispatchOrigin] = gitCredential{username: "public-fixture-bot", token: "public-fixture-transport-token"}
	d.srv.actorGitDomain = ""
	d.srv.actorModels = actormodel.Models{}
	actor, err := d.srv.store.GetManagedActor(context.Background(), allocation.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	return d, intent, actor
}

func ephemeralBootstrapSourceRequest(d *dispatchServer, actor store.ManagedActor, protocols []string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, workspacePath(actor), nil)
	request.Header.Set("Authorization", "Bearer "+guestToken(d.srv.managed.registry.relaySecret, actor.UID, actor.Incarnation))
	if protocols != nil {
		request.Header[relay.WorkspaceBootstrapHeader] = protocols
	}
	response := httptest.NewRecorder()
	d.srv.Handler().ServeHTTP(response, request)
	return response
}

func TestEphemeralBootstrapSourceIsNarrowBeforeReadyAndProof(t *testing.T) {
	for _, cold := range []bool{false, true} {
		name := "ready"
		if cold {
			name = "provisioning"
		}
		t.Run(name, func(t *testing.T) {
			d, intent, actor := ephemeralBootstrapSourceFixture(t)
			if cold {
				raw, err := sql.Open("sqlite", d.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				_, err = raw.Exec(`UPDATE managed_actors SET phase='provisioning',stage='start' WHERE uid=?`, actor.UID)
				raw.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			before, messages := d.calls()
			response := ephemeralBootstrapSourceRequest(d, actor, []string{"ephemeral-v1"})
			var body map[string]any
			want := map[string]any{"origin": intent.Origin, "mode": "gateway", "ephemeral": map[string]any{"commit": intent.SourceCommit}}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body, want) {
				t.Fatalf("wrong narrow bootstrap: status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Set-Cookie") != "" {
				t.Fatal("bootstrap response lost machine-route isolation")
			}
			for _, forbidden := range []string{intent.Payload, intent.ID, intent.Parent.RequestID, actor.OwnerNonce, "public-fixture-transport-token", "root-only"} {
				if forbidden != "" && strings.Contains(response.Body.String(), forbidden) {
					t.Fatalf("bootstrap exposed private child data: %q", forbidden)
				}
			}
			if _, err := d.srv.store.GetEphemeralChildAttestation(context.Background(), intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("source read fabricated proof: %v", err)
			}
			after, sent := d.calls()
			if len(after) != len(before) || len(sent) != len(messages) {
				t.Fatal("bootstrap read invoked guest effects")
			}
		})
	}
}

func TestEphemeralBootstrapSourceRejectsLegacyAndStaleScope(t *testing.T) {
	for _, scenario := range []string{"legacy", "version", "duplicate", "parent", "interrupted", "stopped", "owner", "pool", "binding", "unbound", "git-access"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, actor := ephemeralBootstrapSourceFixture(t)
			ctx := context.Background()
			protocols := []string{"ephemeral-v1"}
			switch scenario {
			case "legacy":
				protocols = nil
			case "version":
				protocols = []string{"ephemeral-v999"}
			case "duplicate":
				protocols = []string{"ephemeral-v1", "ephemeral-v1"}
			case "parent", "binding", "unbound":
				raw, err := sql.Open("sqlite", d.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				query, value := `UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, intent.Parent.Principal.ActorUID
				if scenario == "binding" {
					query, value = `UPDATE managed_actors SET source_fingerprint='foreign' WHERE uid=?`, actor.UID
				}
				if scenario == "unbound" {
					query, value = `DROP TRIGGER ephemeral_child_allocation_delete_guard; DELETE FROM ephemeral_child_allocations WHERE actor_uid=?`, actor.UID
				}
				_, err = raw.Exec(query, value)
				raw.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "interrupted":
				sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
				if err != nil {
					t.Fatal(err)
				}
				_, err = d.srv.store.AcquireOwnerActorOperation(ctx, d.master, "owner-input", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
			case "stopped":
				sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
				if err != nil {
					t.Fatal(err)
				}
				_, err = d.srv.store.StopSwarm(ctx, d.swarm, sw.OwnerID, func(context.Context) bool { return true })
				if err != nil {
					t.Fatal(err)
				}
			case "owner":
				sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.srv.store.SetUserDisabled(ctx, sw.OwnerID, true); err != nil {
					t.Fatal(err)
				}
			case "pool":
				pool, err := d.srv.store.GetManagedPool(ctx, actor.PoolUID)
				if err != nil {
					t.Fatal(err)
				}
				pool.State = "draining"
				if _, err := d.srv.store.UpdateManagedPool(ctx, pool, pool.Revision); err != nil {
					t.Fatal(err)
				}
			case "git-access":
				d.srv.git.remotes = gitRemotes{}
			}
			response := ephemeralBootstrapSourceRequest(d, actor, protocols)
			if response.Code != http.StatusForbidden {
				t.Fatalf("unsafe bootstrap accepted: status=%d body=%s", response.Code, response.Body.String())
			}
			for _, secret := range []string{intent.Payload, intent.Origin, intent.SourceCommit, actor.OwnerNonce} {
				if secret != "" && strings.Contains(response.Body.String(), secret) {
					t.Fatal("refused bootstrap exposed source or private state")
				}
			}
		})
	}
}

func TestEphemeralBootstrapSourcePairsWithRealTLSGuestReceiver(t *testing.T) {
	d, intent, actor := ephemeralBootstrapSourceFixture(t)
	server := httptest.NewTLSServer(d.srv.Handler())
	defer server.Close()
	config := relay.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https") + "/api/proxmox/guest/" + actor.UID + "/1", ActorUID: actor.UID, Generation: actor.Incarnation, Token: guestToken(d.srv.managed.registry.relaySecret, actor.UID, actor.Incarnation), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
	origin, err := guest.FetchWorkspaceBootstrap(context.Background(), config, nil)
	if err != nil || origin.Ephemeral == nil || origin.Ephemeral.Commit != intent.SourceCommit || origin.Origin != intent.Origin || origin.Mode != "gateway" {
		t.Fatalf("producer/receiver bootstrap contract differs: %+v %v", origin, err)
	}
	legacy, err := guest.FetchWorkspaceOrigin(context.Background(), config, nil)
	if err == nil || legacy != (guest.WorkspaceOrigin{}) {
		t.Fatalf("legacy guest obtained unsafe bootstrap: %+v %v", legacy, err)
	}
}

func TestPersistentBootstrapSourceKeepsLegacyWireWithEitherHeader(t *testing.T) {
	d, _, _ := ephemeralBootstrapSourceFixture(t)
	for _, protocol := range [][]string{nil, {"ephemeral-v1"}} {
		response := ephemeralBootstrapSourceRequest(d, d.worker, protocol)
		var body map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatalf("persistent source changed: status=%d body=%s", response.Code, response.Body.String())
		}
		if _, present := body["ephemeral"]; present {
			t.Fatal("persistent actor was relabeled ephemeral")
		}
	}
}
