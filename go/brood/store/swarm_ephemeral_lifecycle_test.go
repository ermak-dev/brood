package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// FEATURE: Durable provenance автоматических first-start/cleanup и stage boundary (#292).
// SPEC: Только exact allocation-bound operation считается автоматической;
// FULL begin и requested-marker CAS предшествуют эффектам, история не даёт permission.
// IMPLEMENTATION: Immutable provenance и существующие managed stages в отдельных FULL tx.
// RESULT: IMPLEMENTED — begin, retry, history, CAS и sealed cleanup проходят Store/race проверки.
// STATUS: DONE
func lifecycleFixture(t *testing.T, kind string) (*Store, EphemeralChildIntent) {
	t.Helper()
	ctx := context.Background()
	if kind == "cleanup" {
		s, intent, _, _ := settledEphemeralResult(t)
		mustNil(t, s.SealEphemeralChildResult(ctx, intent.ID, childAllow))
		return s, intent
	}
	s, intent := createGrantFixture(t)
	_, err := s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
	mustNil(t, err)
	a, err := s.GetManagedActor(ctx, "isolated")
	mustNil(t, err)
	a, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, "create", "retained-create-task")
	mustNil(t, err)
	_, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "create", "stopped")
	mustNil(t, err)
	return s, intent
}

func beginLifecycle(s *Store, ctx context.Context, id, kind string, auth SwarmChildAuthorize) (ManagedActor, error) {
	if kind == "start" {
		return s.BeginEphemeralChildStart(ctx, id, auth)
	}
	return s.BeginEphemeralChildCleanup(ctx, id, auth)
}

func TestEphemeralLifecycleFullBeginRetryCheckAndStageCAS(t *testing.T) {
	for _, kind := range []string{"start", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, intent := lifecycleFixture(t, kind)
			action := "begin_ephemeral_" + kind
			auth := func(ctx context.Context, a SwarmChildAuthorization) error {
				var mode int
				mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
				if a.Action != action || a.Parent != intent.Parent || a.Caller != intent.Parent.Principal || a.RequestID != intent.RequestID || a.Payload != intent.Payload || a.Executor.ActorUID != "isolated" || a.Executor.Incarnation != 1 || !sameChildScope(a.Executor, intent.Parent.Principal) {
					t.Fatal("incorrect lifecycle authority scope")
				}
				if action != "check_ephemeral_"+kind && mode != 2 {
					t.Fatal("lifecycle write not FULL")
				}
				return nil
			}
			old, err := s.GetManagedActor(ctx, "isolated")
			mustNil(t, err)
			if _, err := s.GetEphemeralChildLifecycle(ctx, old.UID, old.Incarnation, old.OperationID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("old operation classified automatic: %v", err)
			}
			a, err := beginLifecycle(s, ctx, intent.ID, kind, auth)
			mustNil(t, err)
			if a.OperationID == old.OperationID || a.UPID != "" || (kind == "start" && (a.Stage != "start" || a.Phase != "stopped" || a.Desired != "running")) || (kind == "cleanup" && (a.Stage != "stop" || a.Operation != "delete" || a.Unsent() != ManagedStopUnsent || a.Desired != "deleted")) {
				t.Fatalf("bad initial operation: %+v", a)
			}
			h, err := s.GetEphemeralChildLifecycle(ctx, a.UID, a.Incarnation, a.OperationID)
			mustNil(t, err)
			if h.IntentID != intent.ID || h.Kind != kind || h.OperationID != a.OperationID || h.ActorUID != a.UID || h.ActorIncarnation != 1 || h.CreatedAt == "" {
				t.Fatal("incorrect immutable provenance")
			}
			before := databaseRows(t, s.db)
			retry, err := beginLifecycle(s, ctx, intent.ID, kind, auth)
			mustNil(t, err)
			if retry != a {
				t.Fatal("retry replaced automatic operation")
			}
			action = "check_ephemeral_" + kind
			mustNil(t, s.CheckEphemeralChildLifecycle(ctx, a.UID, a.Incarnation, a.OperationID, auth))
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("history/check/retry wrote data")
			}
			stage := a.Stage
			action = "reserve_ephemeral_" + kind + "_" + stage
			requested, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, a.Incarnation, a.OperationID, stage, a.Revision, auth)
			mustNil(t, err)
			if requested.Revision != a.Revision+1 || requested.UPID != "" || requested.Error != "" || (kind == "start" && requested.Phase != "provisioning") || (kind == "cleanup" && requested.Phase != "deleting") {
				t.Fatal("request marker not recorded")
			}
			if _, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, stage, requested.Revision, auth); err == nil {
				t.Fatal("unknown submission repeated")
			}
			// Явное доверенное no-effect evidence восстанавливает только прежний marker.
			requested.Phase = "stopped"
			if kind == "cleanup" {
				requested.Phase = "running"
			}
			requested, err = s.UpdateManagedActor(ctx, requested, requested.Revision)
			mustNil(t, err)
			requested, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, stage, requested.Revision, auth)
			mustNil(t, err)
			if kind == "cleanup" {
				requested, err = s.BindManagedActorTask(ctx, a.UID, requested.Revision, a.OperationID, "stop", "stop-task")
				mustNil(t, err)
				requested, err = s.ConfirmManagedActorStage(ctx, a.UID, requested.Revision, a.OperationID, "stop", "stopped")
				mustNil(t, err)
				action = "reserve_ephemeral_cleanup_delete"
				requested, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "delete", requested.Revision, auth)
				mustNil(t, err)
				if requested.Unsent() != "" || requested.Phase != "deleting" {
					t.Fatal("delete marker not consumed")
				}
				requested, err = s.MarkManagedActorDeleteUnsent(ctx, a.UID, requested.Revision, a.OperationID, "trusted no-effect")
				mustNil(t, err)
				_, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "delete", requested.Revision, auth)
				mustNil(t, err)
			}
		})
	}
}

func TestEphemeralLifecycleAtomicFailuresAndNesting(t *testing.T) {
	for _, kind := range []string{"start", "cleanup"} {
		for _, failure := range []string{"nil", "denied", "cancel", "tx", "full", "insert", "ignore", "after"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				s, intent := lifecycleFixture(t, kind)
				auth := SwarmChildAuthorize(childAllow)
				switch failure {
				case "nil":
					auth = nil
				case "denied":
					auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
				case "cancel":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					auth = func(context.Context, SwarmChildAuthorization) error { cancel(); return nil }
				case "insert", "ignore", "after":
					timing, result := "BEFORE", "RAISE(ABORT,'injected failure')"
					if failure == "ignore" {
						result = "RAISE(IGNORE)"
					}
					if failure == "after" {
						timing = "AFTER"
					}
					_, err := s.db.Exec(`CREATE TRIGGER fail_lifecycle ` + timing + ` INSERT ON ephemeral_child_lifecycles BEGIN SELECT ` + result + `; END`)
					mustNil(t, err)
				}
				before := databaseRows(t, s.db)
				if failure == "tx" || failure == "full" {
					run := s.WithTx
					if failure == "full" {
						run = s.WithDurableTx
					}
					mustNil(t, run(ctx, func(ctx context.Context) error {
						a, err := beginLifecycle(s, ctx, intent.ID, kind, auth)
						if !errors.Is(err, ErrDurableTransactionNested) || a != (ManagedActor{}) {
							t.Fatal("nested begin succeeded")
						}
						return nil
					}))
				} else {
					a, err := beginLifecycle(s, ctx, intent.ID, kind, auth)
					if err == nil || a != (ManagedActor{}) {
						t.Fatal("failed begin returned operation")
					}
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("failed begin left partial operation/history")
				}
			})
		}
	}
}

func TestEphemeralLifecycleConcurrentBeginAndStage(t *testing.T) {
	for _, kind := range []string{"start", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			s, intent := lifecycleFixture(t, kind)
			ctx := context.Background()
			results := make(chan ManagedActor, 8)
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					a, err := beginLifecycle(s, ctx, intent.ID, kind, childAllow)
					if err != nil {
						t.Error(err)
					}
					results <- a
				})
			}
			wg.Wait()
			close(results)
			var a ManagedActor
			for r := range results {
				if a.UID != "" && a != r {
					t.Fatal("concurrent begin created different operations")
				}
				a = r
			}
			successes := make(chan bool, 8)
			for range 8 {
				wg.Go(func() {
					_, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, a.Stage, a.Revision, childAllow)
					successes <- err == nil
				})
			}
			wg.Wait()
			close(successes)
			n := 0
			for ok := range successes {
				if ok {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d successful stage reservations", n)
			}
		})
	}
}

func TestEphemeralLifecycleStartThroughSealedCleanup(t *testing.T) {
	ctx := context.Background()
	s, intent := lifecycleFixture(t, "start")
	a, err := s.BeginEphemeralChildStart(ctx, intent.ID, childAllow)
	mustNil(t, err)
	start, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID)
	mustNil(t, err)
	// Source attestation по-прежнему требует ready, а не start intent.
	if _, err := s.ReserveEphemeralChildAttestation(ctx, intent.ID, "not-ready", childAllow); err == nil {
		t.Fatal("automatic start intent bypassed ready-only attestation")
	}
	a, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "start", a.Revision, childAllow)
	mustNil(t, err)
	a, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, "start", "trusted-start-task")
	mustNil(t, err)
	a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "start", "running")
	mustNil(t, err)
	attestForRegistration(t, s, intent)
	child, err := s.RegisterEphemeralChild(ctx, intent.ID, childAllow)
	mustNil(t, err)
	in := ActorDeliveryInput{SwarmID: child.Executor.SwarmID, SourceKind: "child", SourceID: child.ID, RequestID: intent.RequestID, Commit: intent.SourceCommit, ActorUID: a.UID, Incarnation: 1, AtespaceUID: a.AtespaceUID, JournalID: ephemeralResultHash("lifecycle-instruction"), Payload: `{"to":"isolated","body":"Independent task","waitSec":0}`}
	_, err = s.PrepareEphemeralChildDelivery(ctx, intent.ID, in, childAllow)
	mustNil(t, err)
	_, err = s.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision+1, "success", "Retained independent child result", childAllow)
	mustNil(t, err)
	_, err = s.JoinSwarmChildren(ctx, intent.Parent, childAllow)
	mustNil(t, err)
	mustNil(t, s.SealEphemeralChildResult(ctx, intent.ID, childAllow))
	a, err = s.BeginEphemeralChildCleanup(ctx, intent.ID, childAllow)
	mustNil(t, err)
	cleanup, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID)
	mustNil(t, err)
	if cleanup.Kind != "cleanup" || cleanup.OperationID == start.OperationID || cleanup.IntentID != start.IntentID {
		t.Fatal("cleanup rebound automatic first-start provenance")
	}
	retained, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, start.OperationID)
	mustNil(t, err)
	if retained != start {
		t.Fatal("cleanup rewrote first-start history")
	}
	if err := s.CheckEphemeralChildLifecycle(ctx, a.UID, 1, start.OperationID, childAllow); err == nil {
		t.Fatal("historical start remained effect authority after cleanup replacement")
	}
	_, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, childAllow)
	mustNil(t, err)
}

func TestEphemeralLifecycleMigration53PreservesAllRows(t *testing.T) {
	path := loadSchemaFixture(t, 53)
	raw := rawDatabase(t, path)
	before := withEmptyPolicyReviewTables(databaseRows(t, raw))
	before["ephemeral_child_lifecycles"] = nil
	before["ephemeral_child_failures"] = nil
	before["owner_managed_actor_lineage"] = nil
	before["ephemeral_child_executor_failures"] = nil
	before["swarm_eval_bindings"] = nil
	before["swarm_eval_replays"] = nil
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("migration invented provenance or changed old rows")
	}
	requireIntact(t, s.db)
}
