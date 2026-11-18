package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestEphemeralResultSealFailuresNeverAcknowledgeRetention(t *testing.T) {
	for _, scenario := range []string{"nil", "denied", "cancelled", "cancel-callback", "ordinary-tx", "durable-tx", "before-insert", "after-insert", "ignored-insert"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, intent, _, _ := settledEphemeralResult(t)
			auth := SwarmChildAuthorize(childAllow)
			switch scenario {
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "cancelled", "cancel-callback":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				if scenario == "cancelled" {
					cancel()
				} else {
					auth = func(context.Context, SwarmChildAuthorization) error { cancel(); return nil }
				}
			case "before-insert", "after-insert", "ignored-insert":
				statement := `CREATE TRIGGER refuse_result_seal BEFORE INSERT ON ephemeral_child_result_seals BEGIN SELECT RAISE(ABORT,'injected result persistence failure'); END`
				if scenario == "after-insert" {
					statement = `CREATE TRIGGER refuse_result_seal AFTER INSERT ON ephemeral_child_result_seals BEGIN SELECT RAISE(ABORT,'injected result persistence failure'); END`
				} else if scenario == "ignored-insert" {
					statement = `CREATE TRIGGER refuse_result_seal BEFORE INSERT ON ephemeral_child_result_seals BEGIN SELECT RAISE(IGNORE); END`
				}
				_, err := s.db.Exec(statement)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			if scenario == "ordinary-tx" || scenario == "durable-tx" {
				run := s.WithTx
				if scenario == "durable-tx" {
					run = s.WithDurableTx
				}
				mustNil(t, run(ctx, func(ctx context.Context) error {
					if err := s.SealEphemeralChildResult(ctx, intent.ID, auth); !errors.Is(err, ErrDurableTransactionNested) {
						t.Fatalf("nested seal acknowledged before outer commit: %v", err)
					}
					return nil
				}))
			} else if err := s.SealEphemeralChildResult(ctx, intent.ID, auth); err == nil {
				t.Fatal("failed seal acknowledged retention")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("failed result seal changed history or lifecycle")
			}
		})
	}
}

func TestEphemeralResultSealRechecksCurrentFactsAndHolds(t *testing.T) {
	for _, scenario := range []string{"incarnation", "parent-incarnation", "namespace", "project", "parent-report", "child-report", "pass-source", "proof", "unknown-workspace", "parent-owner", "executor-owner"} {
		for _, sealed := range []bool{false, true} {
			t.Run(scenario+map[bool]string{false: "/new", true: "/retained"}[sealed], func(t *testing.T) {
				ctx := context.Background()
				s, intent, child, _ := settledEphemeralResult(t)
				if sealed {
					mustNil(t, s.SealEphemeralChildResult(ctx, intent.ID, childAllow))
				}
				exec := func(query string, args ...any) { t.Helper(); _, err := s.db.Exec(query, args...); mustNil(t, err) }
				switch scenario {
				case "incarnation":
					setActor(t, s, child.Executor.ActorUID, "incarnation=2")
				case "parent-incarnation":
					setActor(t, s, intent.Parent.Principal.ActorUID, "incarnation=2")
				case "namespace":
					exec(`UPDATE swarms SET atespace_uid='replacement' WHERE id=?`, child.Executor.SwarmID)
				case "project":
					seedProject(t, s, "replacement", "Replacement")
					exec(`UPDATE swarms SET project_id='replacement' WHERE id=?`, child.Executor.SwarmID)
				case "parent-report":
					// Отдельно от SQL fence проверяем отказ API при повреждённом snapshot.
					exec(`DROP TRIGGER swarm_steps_report_fence`)
					exec(`UPDATE swarm_steps SET report='Changed parent report' WHERE swarm_id=? AND sequence=?`, child.Executor.SwarmID, intent.Parent.Sequence)
				case "child-report":
					exec(`DROP TRIGGER swarm_child_tasks_fence`)
					exec(`UPDATE swarm_child_tasks SET report='Changed child report' WHERE id=?`, child.ID)
				case "pass-source":
					exec(`DROP TRIGGER swarm_steps_pass_fence; DROP TRIGGER swarm_steps_child_identity`)
					exec(`UPDATE swarm_steps SET source_commit=? WHERE swarm_id=? AND sequence=?`, strings.Repeat("f", 40), child.Executor.SwarmID, intent.Parent.Pass)
				case "proof":
					exec(`DROP TRIGGER ephemeral_child_attestation_update_guard`)
					exec(`UPDATE ephemeral_child_attestations SET state='unconfirmed',attested_origin='',attested_commit='' WHERE intent_id=?`, intent.ID)
				case "unknown-workspace":
					op := ActorOperation{ProjectID: child.Executor.ProjectID, SwarmID: child.Executor.SwarmID, UID: child.Executor.ActorUID, Generation: child.Executor.Incarnation, OperationID: "unknown-workspace", Caller: "trusted", Operation: "workspace_attest", State: "unconfirmed"}
					exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, encodeManaged(op), op.UID)
				case "parent-owner", "executor-owner":
					uid := child.Executor.ActorUID
					if scenario == "parent-owner" {
						uid = intent.Parent.Principal.ActorUID
					}
					actor, err := s.GetManagedActor(ctx, uid)
					mustNil(t, err)
					swarm, err := s.GetSwarm(ctx, child.Executor.SwarmID)
					mustNil(t, err)
					op, err := s.AcquireOwnerActorOperation(ctx, actor, "explicit-owner", "human", "session_terminal", swarm.OwnerID, func(context.Context) error { return nil })
					mustNil(t, err)
					mustNil(t, s.FinishActorOperation(ctx, op, true))
				}
				before := databaseRows(t, s.db)
				if err := s.SealEphemeralChildResult(ctx, intent.ID, childAllow); err == nil {
					t.Fatal("changed facts or current authority acknowledged result seal")
				}
				if err := s.CheckEphemeralChildResultSeal(ctx, intent.ID, childAllow); err == nil {
					t.Fatal("read-only evidence check ignored changed facts")
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("refused seal or check changed retained evidence")
				}
			})
		}
	}
}

func TestEphemeralResultSealConcurrentRetriesAndHistoricalRead(t *testing.T) {
	ctx := context.Background()
	s, intent, child, _ := settledEphemeralResult(t)
	if err := s.CheckEphemeralChildResultSeal(ctx, intent.ID, childAllow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsealed report passed evidence check: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.SealEphemeralChildResult(ctx, intent.ID, childAllow); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	seal, err := s.GetEphemeralChildResultSeal(ctx, intent.ID)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	mustNil(t, s.CheckEphemeralChildResultSeal(ctx, intent.ID, childAllow))
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("read-only check changed evidence")
	}
	for _, query := range []string{
		`UPDATE ephemeral_child_result_seals SET report_hash=parent_report_hash WHERE intent_id=?`,
		`DELETE FROM ephemeral_child_result_seals WHERE intent_id=?`,
		`INSERT OR REPLACE INTO ephemeral_child_result_seals SELECT * FROM ephemeral_child_result_seals WHERE intent_id=?`,
	} {
		if _, err := s.db.Exec(query, intent.ID); err == nil {
			t.Fatal("immutable external evidence was rewritten")
		}
	}
	// Удаление только inventory в тесте: ни подтверждения физического cleanup, ни его разрешения.
	_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid=?`, child.Executor.ActorUID)
	mustNil(t, err)
	history, err := s.GetEphemeralChildResultSeal(ctx, intent.ID)
	mustNil(t, err)
	if history != seal {
		t.Fatal("external result evidence depended on live actor inventory")
	}
	if err := s.CheckEphemeralChildResultSeal(ctx, intent.ID, childAllow); err == nil {
		t.Fatal("history getter became current lifecycle authority")
	}
}
