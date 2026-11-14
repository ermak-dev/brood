package server

// BUG: Отвязанный чат получает предупреждение по непустому результату без нового намерения (#299).
// HYPOTHESIS: Outcome принят за доказательство эффекта, хотя stop уже остановленного актора ничего не пишет.
// SOLUTION: Отсутствие нового намерения доказывает исполнитель, независимо от Outcome.
// RESULT: FIXED — no-op молчит после отвязки; отказ без набора не меняет актора.
// STATUS: FIXED

import (
	"context"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox"
)

func TestTelegramMissingReleaseUpdateRefusalHasNoEffect(t *testing.T) {
	f := telegramActorSetup(t)
	before := f.actor(t)
	writes := lifecycleProviderWrites(f.pve)
	ask := f.chat.confirmation("/actor alpha coding update")
	got := f.chat.press("q-no-release", ask, 0).only(t)
	expectText(t, "missing release update refusal", got, `brood refused to update actor "coding" in swarm "alpha": update refused: the server has no verified guest release (BROOD_ARTIFACTS_DIR); a restart would leave the actor stopped`)
	if !reflect.DeepEqual(f.actor(t), before) || lifecycleProviderWrites(f.pve) != writes {
		t.Fatal("missing release update changed the actor or called the provider")
	}
}

func TestTelegramRevokedNoopLifecycleIsSilent(t *testing.T) {
	for _, action := range []string{"stop", "start"} {
		t.Run(action, func(t *testing.T) {
			f := telegramActorSetup(t)
			if action == "stop" {
				_, err := f.ts.srv.executeOperatorCommand(context.Background(), f.aliceID, f.swarm, "user:"+f.aliceID, coordinationInvocation("stop", map[string]any{"name": "coding"}))
				if err != nil {
					t.Fatal(err)
				}
				settleActor(t, f.ts, f.uid, actorStopped)
			}
			ask := f.chat.confirmation("/actor alpha coding " + action)
			before := f.actor(t)
			writes := lifecycleProviderWrites(f.pve)
			var hooks hookErrors
			clear(f.ts.srv.managed.clients)
			f.ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
				f.unlink(&hooks)()
				return f.pve, nil
			}
			got := f.chat.press("q-noop", ask, 0)
			hooks.check(t)
			after := f.actor(t)
			if !reflect.DeepEqual(after, before) || lifecycleProviderWrites(f.pve) != writes {
				t.Fatal("no-op proof expected no new intent and no provider writes")
			}
			if len(got.sent) != 0 {
				t.Fatalf("revoked no-op chat received reply without new intent: action=%s sent=%+v", action, got.sent)
			}
		})
	}
}
