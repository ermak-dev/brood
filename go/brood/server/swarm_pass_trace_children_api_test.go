package server

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// BUG: Human export теряет сохранённые typed child instructions (#293).
// HYPOTHESIS: Явный DTO шага не проецирует новую child delivery history.
// SOLUTION: Явная childInstructions-проекция использует общий безопасный delivery DTO.
// RESULT: FIXED — export сохраняет frame/попытки без private authority и эффектов.
// STATUS: FIXED
func TestPassTraceAPIExportsChildFrameAndUnknownProbeWithoutAuthority(t *testing.T) {
	d, actor, child := runningEphemeralChildReportFixture(t)
	ctx := context.Background()
	intent, err := d.srv.store.GetActorDeliveryForSource(ctx, d.swarm, "child", child.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	allow := func(context.Context) bool { return true }
	attempt, err := d.srv.store.ReserveActorDeliveryAttempt(ctx, intent.ID, now, now.Add(time.Second), allow)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.srv.store.RecordActorDeliveryOutcome(ctx, attempt, "unconfirmed", "Lost receipt", now, allow); err != nil {
		t.Fatal(err)
	}
	_, err = d.srv.store.ReserveActorDeliveryAttempt(ctx, intent.ID, now.Add(time.Hour), now.Add(time.Hour+time.Second), allow)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.srv.managed.completeChild(ctx, actor, child.ID, child.RequestID, "success", "Independent child evidence"); err != nil {
		t.Fatal(err)
	}
	rawDB, err := sql.Open("sqlite", d.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Отсутствие inventory моделируется в тесте, не разрешает физическое удаление.
	_, err = rawDB.Exec(`DELETE FROM managed_actors WHERE uid=?`, actor.UID)
	rawDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, messages := d.calls()
	path := "/api/swarms/" + d.swarm + "/passes/1"
	response, body, raw := d.do(d.alice, "GET", path, "", nil)
	expectStatus(t, "retained child trace", response, body, http.StatusOK)
	steps, ok := body["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("wrong pass steps: %s", raw)
	}
	step := steps[0].(map[string]any)
	instructions, ok := step["childInstructions"].([]any)
	if !ok || len(instructions) != 1 {
		t.Fatalf("trace API dropped retained child instruction: %s", raw)
	}
	delivery := instructions[0].(map[string]any)
	if delivery["id"] != intent.ID || delivery["sourceKind"] != "child" || delivery["sourceId"] != child.ID || delivery["actorUid"] != actor.UID || delivery["actorIncarnation"] != float64(actor.Incarnation) || delivery["frame"] != intent.Input.Payload {
		t.Fatalf("trace rewrote child identity or original frame: %+v", delivery)
	}
	attempts, ok := delivery["attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("missing child attempt history: %+v", delivery)
	}
	first, second := attempts[0].(map[string]any), attempts[1].(map[string]any)
	if first["mode"] != "send" || first["outcome"] != "unconfirmed" || first["resultRetained"] != true || second["mode"] != "probe" || second["resultRetained"] != false || second["outcome"] != nil || second["observedAt"] != nil {
		t.Fatalf("transport history invented probe evidence: %+v", attempts)
	}
	for _, forbidden := range []string{"lease", "challenge", "signature", "nonce", "journalId", "JournalID", "credentialRef"} {
		if strings.Contains(raw, `"`+forbidden+`"`) {
			t.Fatalf("private authority field leaked: %s", forbidden)
		}
	}
	response, downloaded, _ := d.do(d.alice, "GET", path+"?download=1", "", nil)
	expectStatus(t, "child trace download", response, downloaded, http.StatusOK)
	if !reflect.DeepEqual(downloaded, body) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("download changed or cached private child evidence")
	}
	bob := newClient(t)
	d.login(bob, "bob", "bob-password-1")
	response, body, _ = d.do(bob, "GET", path, "", nil)
	expectStatus(t, "hidden child instruction", response, body, http.StatusNotFound)
	after, sent := d.calls()
	if len(after) != len(before) || len(sent) != len(messages) {
		t.Fatal("export reattested or delivered child work")
	}
}
