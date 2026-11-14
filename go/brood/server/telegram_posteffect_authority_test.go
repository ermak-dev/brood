package server

// BUG: Отвязка после принятого Stop скрывает предупреждение при отказе записи UPID (#299).
// HYPOTHESIS: Пустой результат с ошибкой ошибочно считается доказательством отсутствия намерения.
// SOLUTION: Адаптер использует доказательство у commit намерения, отдельное от результата команды.
// RESULT: FIXED — после принятого Stop/Start и отказа записи UPID предупреждение остаётся.
// STATUS: FIXED

import (
	"context"
	"testing"
)

func TestTelegramUnrecordedStopWarnsLinkedChat(t *testing.T) {
	for _, action := range []string{"stop", "reset"} {
		t.Run(action, func(t *testing.T) {
			f := telegramActorSetup(t)
			ask := f.chat.confirmation("/actor alpha coding " + action)
			before := f.actor(t)
			writes := lifecycleProviderWrites(f.pve)
			f.pve.mu.Lock()
			f.pve.afterStop = func() { execDB(t, f.ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, f.uid) }
			f.pve.mu.Unlock()
			got := f.chat.press("q-linked-posteffect", ask, 0).only(t)
			expectText(t, "unrecorded stop over live link", got, `The outcome of the `+action+` of actor "coding" in swarm "alpha" is unknown: it may have taken effect. Check the actor in brood before you try again.`)
			after := f.actor(t)
			if after.OperationID == before.OperationID || after.Stage != "stop" || after.UPID != "" || lifecycleProviderWrites(f.pve)[2] != writes[2]+1 {
				t.Fatalf("invalid post-effect proof: actor=%+v writes=%v -> %v", after, writes, lifecycleProviderWrites(f.pve))
			}
		})
	}
}

func TestTelegramRevokedUnrecordedStopStillWarns(t *testing.T) {
	for _, action := range []string{"stop", "reset", "update", "start"} {
		t.Run(action, func(t *testing.T) {
			f := telegramActorSetup(t)
			if action == "update" {
				f.ts.srv.artifactsDir = writeGuestArtifacts(t)
				guestReleaseHub(f.ts, reportsRelease(oldRelease))
			}
			if action == "start" {
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
			failBinding := func() {
				execDB(t, f.ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, f.uid)
				f.unlink(&hooks)()
			}
			f.pve.mu.Lock()
			if action == "start" {
				f.pve.afterStart = failBinding
			} else {
				f.pve.afterStop = failBinding
			}
			f.pve.mu.Unlock()
			got := f.chat.press("q-posteffect", ask, 0)
			hooks.check(t)
			after := f.actor(t)
			afterWrites := lifecycleProviderWrites(f.pve)
			stage, writeIndex := "stop", 2
			if action == "start" {
				stage, writeIndex = "start", 1
			}
			if after.OperationID == before.OperationID || after.Stage != stage || after.UPID != "" || afterWrites[writeIndex] != writes[writeIndex]+1 {
				t.Fatalf("invalid effect proof: operation=%s -> %s stage=%s upid=%s revision=%d -> %d writes=%v -> %v", before.OperationID, after.OperationID, after.Stage, after.UPID, before.Revision, after.Revision, writes, afterWrites)
			}
			if len(got.sent) != 1 {
				t.Fatalf("post-effect warning lost: action=%s sent=%+v intent_changed=%v stage=%s upid=%q revision=%d -> %d writes=%v -> %v", action, got.sent, after.OperationID != before.OperationID, after.Stage, after.UPID, before.Revision, after.Revision, writes, afterWrites)
			}
			expectText(t, "unrecorded stop over ended link", got.sent[0].Text, `This chat is no longer linked to your brood account: the `+action+` of actor "coding" in swarm "alpha" may have taken effect. Check the actor in brood.`)
		})
	}
}
