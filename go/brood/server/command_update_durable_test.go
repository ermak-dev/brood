package server

// BUG: Update принимает precommit snapshot за сохранённое намерение (#299).
// HYPOTHESIS: Fallback после несовпавшего OperationID сохраняет старый маркер незапрошенного Stop.
// SOLUTION: Принятие и обещания reconcile требуют успеха либо совпавшего durable чтения.
// RESULT: FIXED — rollback и смена OperationID после Stop дают unknown без обещания повтора.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

func TestUpdateHTTPCommitRollbackIsUnknown(t *testing.T) {
	ts, alice, swarm, before, pve := lifecycleAuthoritySetup(t)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	// Отложенный FK отказывает на commit, уже после заполнения op.intent.
	execDB(t, ts, `CREATE TABLE lifecycle_commit_failure (actor_uid TEXT REFERENCES managed_actors(uid) DEFERRABLE INITIALLY DEFERRED)`)
	execDB(t, ts, `CREATE TRIGGER lifecycle_commit_failure AFTER UPDATE OF operation_id ON managed_actors WHEN NEW.operation_id <> OLD.operation_id BEGIN INSERT INTO lifecycle_commit_failure VALUES ('missing-actor'); END`)
	writes := lifecycleProviderWrites(pve)
	response, body, _ := ts.do(alice, "POST", "/api/commands/update", ts.csrf(alice), map[string]any{"swarm_id": swarm, "arguments": map[string]any{"name": before.Name}})
	after, err := ts.srv.store.GetManagedActor(context.Background(), before.UID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || lifecycleProviderWrites(pve) != writes {
		t.Fatalf("rollback changed actor or provider: before=%+v after=%+v writes=%v -> %v", before, after, writes, lifecycleProviderWrites(pve))
	}
	if response.StatusCode < http.StatusBadRequest || response.Header.Get("X-Brood-Command-Outcome") != "unconfirmed" {
		t.Fatalf("rolled-back update reported accepted: status=%d header=%q body=%+v", response.StatusCode, response.Header.Get("X-Brood-Command-Outcome"), body)
	}
	detail, _ := body["detail"].(string)
	expectUpdateDurabilityUnconfirmed(t, detail)
	t.Logf("rollback: HTTP %d, outcome=%q, actor unchanged, provider writes %v -> %v", response.StatusCode, response.Header.Get("X-Brood-Command-Outcome"), writes, lifecycleProviderWrites(pve))
}

func TestUpdateSupersededAfterStopIsUnknown(t *testing.T) {
	ts, _, swarm, before, pve := lifecycleAuthoritySetup(t)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	ctx := context.Background()
	user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	replacement := store.NewID()
	pve.mu.Lock()
	pve.afterStop = func() {
		execDB(t, ts, `UPDATE managed_actors SET revision=revision+1, operation_id=? WHERE uid=?`, replacement, before.UID)
	}
	pve.mu.Unlock()
	writes := lifecycleProviderWrites(pve)
	result, evidence, commandErr := ts.srv.executeOperatorCommandWithEvidence(ctx, user.ID, swarm, "user:"+user.ID, coordinationInvocation("update", map[string]any{"name": before.Name}))
	after, err := ts.srv.store.GetManagedActor(ctx, before.UID)
	if err != nil {
		t.Fatal(err)
	}
	wantWrites := writes
	wantWrites[2]++
	if after.OperationID != replacement || after.UPID != "" || lifecycleProviderWrites(pve) != wantWrites {
		t.Fatalf("invalid post-Stop replacement: actor=%+v writes=%v -> %v", after, writes, lifecycleProviderWrites(pve))
	}
	if commandErr == nil || result.Outcome != "unknown" || evidence != lifecycleIntentAccepted {
		t.Fatalf("superseded update reported accepted: result=%+v evidence=%v err=%v", result, evidence, commandErr)
	}
	var data struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.OperationID == "" || data.OperationID == before.OperationID || data.OperationID == replacement || !strings.Contains(commandErr.Error(), data.OperationID) {
		t.Fatalf("unknown outcome lost attempted operation identity: data=%s err=%v", result.Data, commandErr)
	}
	expectUpdateDurabilityUnconfirmed(t, commandErr.Error())
}

func expectUpdateDurabilityUnconfirmed(t *testing.T, text string) {
	t.Helper()
	for _, want := range []string{"update outcome is unknown", "durability is unconfirmed", "reconcile the allocation before another lifecycle change"} {
		if !strings.Contains(text, want) {
			t.Errorf("unconfirmed update lacks %q: %s", want, text)
		}
	}
	for _, falsePromise := range []string{"is retained", "not yet requested", "reconcile requests it on every pass", "not retried automatically"} {
		if strings.Contains(text, falsePromise) {
			t.Errorf("unconfirmed update promises %q: %s", falsePromise, text)
		}
	}
}
