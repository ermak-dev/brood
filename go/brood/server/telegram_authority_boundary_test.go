package server

// BUG: Отвязанный чат получает предупреждение при отказе update до намерения (#299).
// HYPOTHESIS: Адаптер считает любой отказ, кроме доступа, возможным эффектом.
// SOLUTION: Явное доказательство отказа до намерения разрешает молчать; возможный эффект — нет.
// RESULT: FIXED — без намерения нет ответа; неизвестный Stop сохраняет предупреждение.
// STATUS: FIXED

import (
	"reflect"
	"testing"
)

func TestTelegramRevokedBeforeUpdatePreconditionRefusalIsSilent(t *testing.T) {
	f := telegramActorSetup(t)
	ask := f.chat.confirmation("/actor alpha coding update")
	before := f.actor(t)
	writes := lifecycleProviderWrites(f.pve)
	var hooks hookErrors
	f.ts.srv.telegram.commands.beforeWrite = f.unlink(&hooks)
	got := f.chat.press("q-review", ask, 0)
	hooks.check(t)
	after := f.actor(t)
	if !reflect.DeepEqual(after, before) || lifecycleProviderWrites(f.pve) != writes {
		t.Fatal("proof expected no intent and no provider writes")
	}
	if len(got.sent) != 0 {
		t.Fatalf("chat revoked before a known pre-intent refusal still received a reply: sent=%+v revision=%d -> %d writes=%v -> %v", got.sent, before.Revision, after.Revision, writes, lifecycleProviderWrites(f.pve))
	}
}

func TestTelegramActorUnknownOutcomeOverEndedLinkStillNamesPossibleEffect(t *testing.T) {
	f := telegramActorSetup(t)
	f.ts.srv.artifactsDir = writeGuestArtifacts(t)
	guestReleaseHub(f.ts, reportsRelease(oldRelease))
	before := f.actor(t)
	writes := lifecycleProviderWrites(f.pve)
	var hooks hookErrors
	f.pve.mu.Lock()
	f.pve.uncertainStop = true
	f.pve.afterStop = f.unlink(&hooks)
	f.pve.mu.Unlock()
	ask := f.chat.confirmation("/actor alpha coding update")
	got := f.chat.press("q-unknown", ask, 0).only(t)
	hooks.check(t)
	expectText(t, "unknown update over ended link", got,
		`This chat is no longer linked to your brood account: the update of actor "coding" in swarm "alpha" may have taken effect. Check the actor in brood.`)
	after := f.actor(t)
	if after.OperationID == before.OperationID || after.Operation != "start" || after.Stage != "stop" || after.Phase != "error" || after.UPID != "" || lifecycleProviderWrites(f.pve)[2] != writes[2]+1 {
		t.Fatalf("proof expected a retained update intent and attempted stop: actor=%+v -> %+v writes=%v -> %v", before, after, writes, lifecycleProviderWrites(f.pve))
	}
}
