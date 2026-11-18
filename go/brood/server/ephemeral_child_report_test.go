package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// BUG: Ephemeral child_complete подтверждает отчёт без FULL-sync (#292).
// HYPOTHESIS: Общая command-транзакция использует обычный WithTx.
// SOLUTION: Текущая ephemeral class выбирает FULL после внешней preflight-проверки.
// RESULT: FIXED — реальный relay пишет с FULL, вложенное подтверждение запрещено.
// STATUS: FIXED
func runningEphemeralChildReportFixture(t *testing.T) (*dispatchServer, store.ManagedActor, store.SwarmChildTask) {
	t.Helper()
	d, intent, child := ephemeralChildServerFixture(t)
	ctx := context.Background()
	actor, err := d.srv.store.GetManagedActor(ctx, child.Executor.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ephemeralChildInstruction(ephemeralInstructionSource{Intent: intent, Allocation: allocation, Proof: proof, Actor: actor, Work: store.SwarmChildWork{Parent: intent.Parent, Task: child}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(relay.MessageRequest{To: actor.Name, From: stepMessageSender(d.swarm), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.srv.store.PrepareEphemeralChildDelivery(ctx, intent.ID, store.ActorDeliveryInput{SwarmID: d.swarm, SourceKind: "child", SourceID: child.ID, RequestID: intent.RequestID, Commit: intent.SourceCommit, ActorUID: actor.UID, Incarnation: actor.Incarnation, AtespaceUID: actor.AtespaceUID, JournalID: strings.Repeat("d", 64), Payload: string(payload)}, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, actor, work.Task
}

func TestEphemeralChildReportUsesFullWriterThroughRealRelay(t *testing.T) {
	d, actor, child := runningEphemeralChildReportFixture(t)
	raw, err := sql.Open("sqlite", d.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// pragma_synchronous читается именно соединением UPDATE, не наблюдателем.
	_, err = raw.Exec(`CREATE TRIGGER require_full_child_report BEFORE UPDATE OF state ON swarm_child_tasks WHEN NEW.state='completed' AND (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'child report writer requires FULL'); END`)
	if err != nil {
		t.Fatal(err)
	}
	d.worker = actor
	parent, executor, messages := childRelay(t, d)
	report := "Exact independent report\nretained before any cleanup permission."
	response := childCall(t, executor, "child_complete", map[string]any{"child_id": child.ID, "request_id": child.RequestID, "outcome": "success", "report": report})
	if !response.OK {
		t.Fatalf("ephemeral report did not commit with FULL writer: %+v", response)
	}
	for range 2 {
		response = childCall(t, parent, "child_get", map[string]any{"child_id": child.ID})
		var view childTaskJSON
		if !response.OK || json.Unmarshal(response.Data, &view) != nil || view.State != "completed" || view.Outcome != "success" || view.Report != report {
			t.Fatalf("exact externally retained report unavailable: %+v", response)
		}
		response = childCall(t, executor, "child_complete", map[string]any{"child_id": child.ID, "request_id": child.RequestID, "outcome": "success", "report": report})
		if !response.OK {
			t.Fatalf("exact report retry refused: %+v", response)
		}
	}
	var state, outcome, stored string
	var revision int64
	err = raw.QueryRow(`SELECT state,outcome,report,revision FROM swarm_child_tasks WHERE id=?`, child.ID).Scan(&state, &outcome, &stored, &revision)
	if err != nil || state != "completed" || outcome != "success" || stored != report || revision != child.Revision+1 {
		t.Fatalf("external connection observed changed report: %s %s %q %d %v", state, outcome, stored, revision, err)
	}
	current, err := d.srv.store.GetManagedActor(context.Background(), actor.UID)
	if err != nil || current != actor || messages.Load() != 0 {
		t.Fatal("report retention mutated actor lifecycle or delivered work")
	}
}

func TestEphemeralChildReportFailureRetainsRunningWork(t *testing.T) {
	d, actor, child := runningEphemeralChildReportFixture(t)
	raw, err := sql.Open("sqlite", d.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER refuse_child_report AFTER UPDATE OF state ON swarm_child_tasks WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'injected report persistence failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	d.worker = actor
	_, executor, messages := childRelay(t, d)
	args := map[string]any{"child_id": child.ID, "request_id": child.RequestID, "outcome": "fail", "report": "Independent failure evidence"}
	response := childCall(t, executor, "child_complete", args)
	if response.OK || response.Outcome != "unconfirmed" {
		t.Fatalf("failed persistence was acknowledged: %+v", response)
	}
	work, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || work.Task != child || messages.Load() != 0 {
		t.Fatal("failed report changed work or delivered effects")
	}
	if _, err := raw.Exec(`DROP TRIGGER refuse_child_report`); err != nil {
		t.Fatal(err)
	}
	response = childCall(t, executor, "child_complete", args)
	if !response.OK {
		t.Fatalf("retry after rollback could not retain original report: %+v", response)
	}
	args["report"] = "Changed report must not overwrite evidence"
	if response = childCall(t, executor, "child_complete", args); response.OK {
		t.Fatal("changed report overwrote retained evidence")
	}
	work, err = d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
	if err != nil || work.Task.State != "completed" || work.Task.Outcome != "fail" || work.Task.Report != "Independent failure evidence" || work.Task.Revision != child.Revision+1 {
		t.Fatalf("report retry changed history: %+v %v", work, err)
	}
}

func TestPersistentChildReportStillParticipatesInOuterRollback(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	child, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "one"))
	if err != nil {
		t.Fatal(err)
	}
	d.srv.children.dispatch(ctx)
	before, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil || before.Task.State != "running" {
		t.Fatalf("persistent child not running: %+v %v", before, err)
	}
	rollback := errors.New("outer rollback")
	err = d.srv.store.WithTx(ctx, func(ctx context.Context) error {
		if err := d.srv.managed.completeChild(ctx, d.worker, child.ID, child.RequestID, "success", "Uncommitted persistent report"); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	after, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil || after != before {
		t.Fatal("persistent completion escaped its outer rollback")
	}
}

func TestEphemeralChildReportRejectsAllOuterTransactions(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "durable"}[durable], func(t *testing.T) {
			d, actor, child := runningEphemeralChildReportFixture(t)
			// Relay identity не включает class: устаревший snapshot не снижает durability.
			actor.Class = store.ActorClassPersistent
			run := d.srv.store.WithTx
			if durable {
				run = d.srv.store.WithDurableTx
			}
			err := run(context.Background(), func(ctx context.Context) error {
				err := d.srv.managed.completeChild(ctx, actor, child.ID, child.RequestID, "success", "Nested report must not be acknowledged")
				if !errors.Is(err, store.ErrDurableTransactionNested) {
					t.Fatalf("nested report was acknowledged before outer commit: %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			work, err := d.srv.store.SwarmChildWorkByID(context.Background(), child.ID)
			if err != nil || work.Task != child {
				t.Fatal("nested report changed retained child")
			}
		})
	}
}
