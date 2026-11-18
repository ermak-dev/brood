package server

import (
	"context"
	"net/http"
	"testing"

	"brood.dev/go/brood/store"
)

// BUG: Общий inventory не сообщает класс, и панель предлагает ephemeral как роль графа.
// HYPOTHESIS: Явный class позволяет исключить исполнителя только из выбора роли,
// не скрывая его от owner в обычном списке акторов.
// SOLUTION: Inventory отдаёт class; роль выбирается только из persistent.
// RESULT: FIXED — HTTP, race, full suites и реальный desktop/phone Select зелёные.
// STATUS: FIXED
func TestActorInventoryExportsExecutorClassWithoutHidingEphemeral(t *testing.T) {
	ctx := context.Background()
	d, intent, reservation := ephemeralCapacityFixture(t)
	executor, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation)
	if err != nil {
		t.Fatal(err)
	}
	before, sent := d.calls()
	response, body, _ := d.do(d.alice, "GET", "/api/swarms/"+d.swarm+"/actors", "", nil)
	expectStatus(t, "executor classes", response, body, http.StatusOK)
	classes := map[string]any{}
	for _, row := range body["results"].([]any) {
		actor := row.(map[string]any)
		metadata := actor["metadata"].(map[string]any)
		classes[metadata["uid"].(string)] = actor["class"]
	}
	if classes[d.master.UID] != store.ActorClassPersistent || classes[executor.ActorUID] != store.ActorClassEphemeral {
		t.Fatalf("inventory must retain both identities with authoritative classes: %+v", classes)
	}
	after, messages := d.calls()
	if len(before) != len(after) || len(sent) != len(messages) {
		t.Fatal("inventory read caused actor effects")
	}
}
