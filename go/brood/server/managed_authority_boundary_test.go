package server

// BUG: Отзыв сессии старого HTTP-маршрута и роли перед транзакцией не запрещает запись (#299).
// HYPOTHESIS: Маршрут не передаёт сессию, а транзакция не проверяет текущую роль.
// SOLUTION: Общий веб-контекст передаёт сессию; транзакция читает текущие права.
// RESULT: FIXED — все четыре HTTP-действия и понижение роли отвергнуты без изменений.
// STATUS: FIXED

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxmox"
)

func TestManagedLegacyLifecycleRejectsRevokedSession(t *testing.T) {
	for _, action := range []string{"stop", "resume", "reset", "delete"} {
		t.Run(action, func(t *testing.T) {
			ts, alice, swarm, before, pve := lifecycleAuthoritySetup(t)
			ctx := context.Background()
			if action == "resume" {
				r, body, _ := ts.do(alice, "POST", "/api/commands/stop", ts.csrf(alice), map[string]any{"swarm_id": swarm, "arguments": map[string]any{"name": before.Name}})
				expectStatus(t, "initial stop", r, body, http.StatusAccepted)
				before = settleActor(t, ts, before.UID, actorStopped)
			}
			user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			csrf := ts.csrf(alice)
			writes := lifecycleProviderWrites(pve)
			var hooks hookErrors
			revoked := false
			clear(ts.srv.managed.clients)
			ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
				return lifecycleInspectHook{proxmoxAPI: pve, afterInspect: func() {
					if !revoked {
						revoked = true
						_, err := ts.srv.store.DeleteSessionsOfUser(ctx, user.ID)
						hooks.add(err)
					}
				}}, nil
			}
			method := "POST"
			var body any = map[string]string{"action": action}
			if action == "delete" {
				method, body = "DELETE", nil
			}
			r, response, _ := ts.do(alice, method, "/api/swarms/"+swarm+"/actors/"+before.Name, csrf, body)
			hooks.check(t)
			after, err := ts.srv.store.GetManagedActor(ctx, before.UID)
			if err != nil {
				t.Fatal(err)
			}
			if !revoked || r.StatusCode != http.StatusForbidden || !reflect.DeepEqual(after, before) || lifecycleProviderWrites(pve) != writes {
				t.Fatalf("legacy request crossed revocation: action=%s revoked=%v status=%d response=%v revision=%d -> %d writes=%v -> %v", action, revoked, r.StatusCode, response, before.Revision, after.Revision, writes, lifecycleProviderWrites(pve))
			}
		})
	}
}

func TestManagedLifecycleChecksACLInIntentTransaction(t *testing.T) {
	ts, alice, swarm, before, pve := lifecycleAuthoritySetup(t)
	ctx := context.Background()
	user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := ts.srv.store.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetSwarmMember(ctx, swarm, bob.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetSwarmMember(ctx, swarm, user.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	var token string
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
		id, _, loadErr := sessions.Load(ctx, token, time.Now())
		checks++
		if checks == 2 {
			// Последняя внешняя проверка успешна. Моделируем понижение роли,
			// завершённое до транзакции намерения, как существующий тест
			// границы транзакции моделирует отзыв сессии.
			if err := ts.srv.store.SetSwarmMember(ctx, swarm, user.ID, "viewer"); err != nil {
				t.Fatal(err)
			}
		}
		return loadErr == nil && id == user.ID
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
	opErr := op.Apply(nil)
	after, err := ts.srv.store.GetManagedActor(ctx, before.UID)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := ts.srv.app.Can(user.ID, app.ActionManageSwarm, app.SwarmRes(swarm))
	if err != nil {
		t.Fatal(err)
	}
	id, _, sessionErr := sessions.Load(ctx, token, time.Now())
	if allowed || sessionErr != nil || id != user.ID {
		t.Fatalf("invalid proof setup: allowed=%v session=%v user=%s", allowed, sessionErr, id)
	}
	if !errors.Is(opErr, app.ErrNotAllowed) || !reflect.DeepEqual(after, before) || lifecycleProviderWrites(pve) != writes {
		t.Fatalf("intent crossed committed ACL revocation: err=%v checks=%d allowed=%v revision=%d -> %d writes=%v -> %v", opErr, checks, allowed, before.Revision, after.Revision, writes, lifecycleProviderWrites(pve))
	}
}
