package server

// BUG: Отзыв сессии во время проверок не мешает записи намерения актора (#299).
// HYPOTHESIS: Перед записью после сетевых чтений не проверяется сессия команды.
// SOLUTION: Права проверяются после namespace; сессия — там же и в транзакции намерения.
// RESULT: FIXED — отзыв не меняет актора и не отправляет запросы провайдеру; CAS сохранён.
// STATUS: FIXED

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

type lifecycleInspectHook struct {
	proxmoxAPI
	afterInspect func()
}

func (p lifecycleInspectHook) InspectOwned(ctx context.Context, ref proxmox.OwnedCT) (proxmox.CTConfig, error) {
	config, err := p.proxmoxAPI.InspectOwned(ctx, ref)
	if p.afterInspect != nil {
		p.afterInspect()
	}
	return config, err
}

func lifecycleProviderWrites(p *managedFakePVE) [5]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return [5]int{p.creates, p.starts, p.stops, p.deletes, p.bootstraps}
}

func lifecycleAuthoritySetup(t *testing.T) (*substrateServer, *http.Client, string, store.ManagedActor, *managedFakePVE) {
	t.Helper()
	ts, alice, _, swarm, pve, hub := managedSetup(t)
	body := map[string]any{"metadata": map[string]string{"name": "coding"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), body)
	expectStatus(t, "create actor", r, b, http.StatusCreated)
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	a := settleActor(t, ts, b["metadata"].(map[string]any)["uid"].(string), actorRunning)
	return ts, alice, swarm, a, pve
}

func TestManagedLifecycleRejectsRevokedCommandSession(t *testing.T) {
	for _, action := range []string{"stop", "start", "update", "reset"} {
		for _, point := range []string{"preflight namespace", "final namespace", "provider observation"} {
			t.Run(action+"/"+point, func(t *testing.T) {
				ts, alice, swarm, before, pve := lifecycleAuthoritySetup(t)
				ctx := context.Background()
				if action == "start" {
					r, b, _ := ts.do(alice, "POST", "/api/commands/stop", ts.csrf(alice), map[string]any{"swarm_id": swarm, "arguments": map[string]any{"name": before.Name}})
					expectStatus(t, "stop before start", r, b, http.StatusAccepted)
					before = settleActor(t, ts, before.UID, actorStopped)
				}
				if action == "update" {
					ts.srv.artifactsDir = writeGuestArtifacts(t)
					guestReleaseHub(ts, reportsRelease(oldRelease))
				}
				user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
				if err != nil {
					t.Fatal(err)
				}
				csrf := ts.csrf(alice)
				writes := lifecycleProviderWrites(pve)
				var hooks hookErrors
				revoked := false
				revoke := func() {
					if !revoked {
						revoked = true
						_, err := ts.srv.store.DeleteSessionsOfUser(ctx, user.ID)
						hooks.add(err)
					}
				}
				if point == "preflight namespace" {
					ts.fake.mu.Lock()
					ts.fake.beforeNamespaceGet = revoke
					ts.fake.mu.Unlock()
				} else {
					clear(ts.srv.managed.clients)
					ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
						return lifecycleInspectHook{proxmoxAPI: pve, afterInspect: func() {
							if point == "provider observation" {
								revoke()
							} else {
								ts.fake.mu.Lock()
								ts.fake.beforeNamespaceGet = revoke
								ts.fake.mu.Unlock()
							}
						}}, nil
					}
				}
				args := map[string]any{"name": before.Name}
				if action == "reset" {
					args["confirm"] = true
				}
				r, b, _ := ts.do(alice, "POST", "/api/commands/"+action, csrf, map[string]any{"swarm_id": swarm, "arguments": args})
				hooks.check(t)
				after, err := ts.srv.store.GetManagedActor(ctx, before.UID)
				if err != nil {
					t.Fatal(err)
				}
				if !revoked || r.StatusCode != http.StatusForbidden || !reflect.DeepEqual(after, before) || lifecycleProviderWrites(pve) != writes {
					t.Fatalf("revoked command changed lifecycle: revoked=%v status=%d body=%v actor=%+v -> %+v writes=%v -> %v", revoked, r.StatusCode, b, before, after, writes, lifecycleProviderWrites(pve))
				}
			})
		}
	}
}

func TestManagedLifecycleChecksSessionInIntentTransaction(t *testing.T) {
	ts, alice, swarm, before, pve := lifecycleAuthoritySetup(t)
	ctx := context.Background()
	user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	token := ""
	for _, cookie := range alice.Jar.Cookies(endpoint) {
		if cookie.Name == auth.CookieSession {
			token = cookie.Value
		}
	}
	if token == "" {
		t.Fatal("no web session")
	}
	sessions := auth.NewSessions(ts.srv.store, 0)
	checks := 0
	commandCtx := context.WithValue(ctx, commandSessionAuthorityKey{}, func(ctx context.Context) bool {
		id, _, err := sessions.Load(ctx, token, time.Now())
		checks++
		// Отзыв сразу после последней внешней проверки: возвращённое true
		// уже устарело. Только транзакция намерения увидит отсутствие сессии.
		if checks == 2 {
			if _, revokeErr := ts.srv.store.DeleteSessionsOfUser(ctx, user.ID); revokeErr != nil {
				t.Fatal(revokeErr)
			}
		}
		return err == nil && id == user.ID
	})
	sw, err := ts.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := ts.srv.store.GetManagedPool(ctx, before.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	writes := lifecycleProviderWrites(pve)
	op := &managedWorkloadOperation{server: ts.srv, context: commandCtx, userID: user.ID, swarm: toAppSwarm(sw), pool: pool, actor: before, name: before.Name, action: "stop"}
	err = op.Apply(nil)
	after, getErr := ts.srv.store.GetManagedActor(ctx, before.UID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if !errors.Is(err, app.ErrNotAllowed) || !reflect.DeepEqual(after, before) || lifecycleProviderWrites(pve) != writes {
		t.Fatalf("intent write used stale session: err=%v checks=%d revision=%d -> %d writes=%v -> %v", err, checks, before.Revision, after.Revision, writes, lifecycleProviderWrites(pve))
	}
}

func TestManagedLifecycleRechecksCurrentAuthorityAfterNamespace(t *testing.T) {
	ts, _, swarm, before, pve := lifecycleAuthoritySetup(t)
	ctx := context.Background()
	user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	writes := lifecycleProviderWrites(pve)
	var hooks hookErrors
	clear(ts.srv.managed.clients)
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		return lifecycleInspectHook{proxmoxAPI: pve, afterInspect: func() {
			ts.fake.mu.Lock()
			ts.fake.beforeNamespaceGet = func() { hooks.add(ts.srv.store.SetUserDisabled(ctx, user.ID, true)) }
			ts.fake.mu.Unlock()
		}}, nil
	}
	// Без ключа веб-сессии, как у делегированной команды: отказ обязан
	// проверять текущие права, а не полагаться на отзыв браузерной сессии.
	_, commandErr := ts.srv.executeOperatorCommand(ctx, user.ID, swarm, "user:"+user.ID, coordinationInvocation("stop", map[string]any{"name": before.Name}))
	hooks.check(t)
	after, err := ts.srv.store.GetManagedActor(ctx, before.UID)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(commandErr, app.ErrNotAllowed) || !reflect.DeepEqual(after, before) || lifecycleProviderWrites(pve) != writes {
		t.Fatalf("disabled user changed lifecycle: err=%v revision=%d -> %d writes=%v -> %v", commandErr, before.Revision, after.Revision, writes, lifecycleProviderWrites(pve))
	}
}

func TestManagedLifecycleRetainsCASAfterAuthorityCheck(t *testing.T) {
	ts, alice, swarm, before, pve := lifecycleAuthoritySetup(t)
	ctx := context.Background()
	csrf := ts.csrf(alice)
	writes := lifecycleProviderWrites(pve)
	var hooks hookErrors
	var concurrent store.ManagedActor
	clear(ts.srv.managed.clients)
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		return lifecycleInspectHook{proxmoxAPI: pve, afterInspect: func() {
			ts.fake.mu.Lock()
			ts.fake.beforeNamespaceGet = func() {
				var err error
				concurrent, err = ts.srv.store.BeginManagedActorOperation(ctx, before.UID, before.Revision, store.NewID(), "reset")
				hooks.add(err)
			}
			ts.fake.mu.Unlock()
		}}, nil
	}
	r, b, _ := ts.do(alice, "POST", "/api/commands/stop", csrf, map[string]any{"swarm_id": swarm, "arguments": map[string]any{"name": before.Name}})
	hooks.check(t)
	after, err := ts.srv.store.GetManagedActor(ctx, before.UID)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusConflict || concurrent.UID == "" || !reflect.DeepEqual(after, concurrent) || lifecycleProviderWrites(pve) != writes {
		t.Fatalf("concurrent intent overwritten: status=%d body=%v concurrent=%+v after=%+v writes=%v -> %v", r.StatusCode, b, concurrent, after, writes, lifecycleProviderWrites(pve))
	}
}
