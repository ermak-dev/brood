package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// BUG: Отменённый до Prepare зарегистрированный ephemeral навсегда удерживает capacity.
// HYPOTHESIS: Cleanup знает только seal/failure, но не положительный pending→cancelled CAS.
// SOLUTION: Distinct cancel_cleanup требует точный CAS, proof и applied computed join.
// RESULT: FIXED — исходный behavioral RED стал GREEN без synthetic report/failure.
// STATUS: FIXED
func cancelledCleanupFixture(t *testing.T, joined bool) (*Store, EphemeralChildIntent, SwarmChildTask) {
	t.Helper()
	ctx := context.Background()
	s, job, child := registeredFailureFixture(t)
	child, err := s.CancelSwarmChild(ctx, job.Parent, child.RequestID, child.Revision, childAllow)
	mustNil(t, err)
	if joined {
		join, err := s.JoinSwarmChildren(ctx, job.Parent, childAllow)
		mustNil(t, err)
		if join.Evaluation.State != "ready_fail" || join.Evaluation.Open != 0 {
			t.Fatalf("not settled: %+v", join)
		}
	}
	return s, job, child
}

func TestEphemeralCancelCleanupAuthorityAndStages(t *testing.T) {
	ctx := context.Background()
	s, job, child := cancelledCleanupFixture(t, true)
	calls := 0
	action := "begin_ephemeral_cleanup"
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if a.Action != action || a.Parent != job.Parent || a.Caller != job.Parent.Principal || a.Executor != child.Executor || a.RequestID != job.RequestID || a.Payload != job.Payload {
			t.Fatalf("wrong authority: %+v", a)
		}
		if action != "check_ephemeral_cleanup" && mode != 2 {
			t.Fatal("not FULL")
		}
		return nil
	}
	a, err := s.BeginEphemeralChildCleanup(ctx, job.ID, auth)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	retry, err := s.BeginEphemeralChildCleanup(ctx, job.ID, auth)
	mustNil(t, err)
	if retry != a || calls != 2 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("retry changed state or skipped authorization")
	}
	action = "check_ephemeral_cleanup"
	mustNil(t, s.CheckEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID, auth))
	if _, err := s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID); err == nil {
		t.Fatal("cancellation bypassed physical absence proof")
	}
	for _, stage := range []string{"stop", "delete"} {
		action = "reserve_ephemeral_cleanup_" + stage
		before = databaseRows(t, s.db)
		if _, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, stage, a.Revision-1, auth); !errors.Is(err, ErrRevisionConflict) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("stale stage revision authorized or wrote state")
		}
		a, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, stage, a.Revision, auth)
		mustNil(t, err)
		before = databaseRows(t, s.db)
		if _, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, stage, a.Revision, auth); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("unknown requested stage rearmed")
		}
		if stage == "stop" {
			a, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, stage, "trusted-stop-task")
			mustNil(t, err)
			a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, stage, "stopped")
			mustNil(t, err)
		}
	}
	action = "begin_ephemeral_cleanup"
	before = databaseRows(t, s.db)
	retry, err = s.BeginEphemeralChildCleanup(ctx, job.ID, auth)
	mustNil(t, err)
	if retry != a || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("begin retry replaced unknown delete")
	}
}

func TestEphemeralCancelCleanupRevocation(t *testing.T) {
	for _, phase := range []string{"begin", "check", "reserve"} {
		for _, change := range []string{"nil", "denied", "reset", "parent-reset", "role", "parent", "pool", "source", "pass-source", "physical", "manual", "workspace", "parent-workspace", "coordination", "intervention", "parent-intervention", "join-report", "join-outcome", "proof", "interruption", "cancel-revision", "cancel-finish", "unknown-stage"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				s, job, child := cancelledCleanupFixture(t, true)
				a, err := s.GetManagedActor(ctx, child.Executor.ActorUID)
				mustNil(t, err)
				if phase != "begin" {
					a, err = s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
					mustNil(t, err)
				}
				auth := SwarmChildAuthorize(childAllow)
				switch change {
				case "nil":
					auth = nil
				case "denied":
					auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
				case "reset":
					setActor(t, s, a.UID, "incarnation=2")
				case "parent-reset":
					setActor(t, s, job.Parent.Principal.ActorUID, "incarnation=2")
				case "role":
					// CHECK уже запрещает такое изменение; scope также защищает
					// старый/corrupt inventory, загруженный без этих ограничений.
					_, err = s.db.Exec(`PRAGMA ignore_check_constraints=ON`)
					mustNil(t, err)
					setActor(t, s, a.UID, "role='master'")
					_, err = s.db.Exec(`PRAGMA ignore_check_constraints=OFF`)
					mustNil(t, err)
				case "parent":
					setActor(t, s, job.Parent.Principal.ActorUID, "phase='stopped'")
				case "pool":
					_, err = s.db.Exec(`UPDATE managed_pools SET source_fingerprint='revoked' WHERE uid='pool'`)
					mustNil(t, err)
				case "source":
					_, err = s.db.Exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://other.example/repo.git'`)
					mustNil(t, err)
				case "pass-source":
					_, err = s.db.Exec(`DROP TRIGGER swarm_steps_pass_fence; UPDATE swarm_steps SET source_commit='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' WHERE source_commit IS NOT NULL`)
					mustNil(t, err)
				case "join-report":
					_, err = s.db.Exec(`DROP TRIGGER swarm_steps_report_fence; UPDATE swarm_steps SET report='not the computed join' WHERE swarm_id=? AND sequence=?`, job.Parent.Principal.SwarmID, job.Parent.Sequence)
					mustNil(t, err)
				case "join-outcome":
					_, err = s.db.Exec(`UPDATE swarm_steps SET outcome='success' WHERE swarm_id=? AND sequence=?`, job.Parent.Principal.SwarmID, job.Parent.Sequence)
					mustNil(t, err)
				case "proof":
					_, err = s.db.Exec(`DROP TRIGGER ephemeral_child_attestation_update_guard; UPDATE ephemeral_child_attestations SET source_commit='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',attested_commit='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'`)
					mustNil(t, err)
				case "interruption":
					_, err = s.db.Exec(`INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES('stop',?,?,'unrelated',1,'u','human','pass_stop','completed','now')`, job.Parent.Principal.ProjectID, job.Parent.Principal.SwarmID)
					mustNil(t, err)
					_, err = s.db.Exec(`INSERT INTO actor_step_interruptions(intervention_id,swarm_id,sequence) VALUES('stop',?,?)`, job.Parent.Principal.SwarmID, job.Parent.Sequence)
					mustNil(t, err)
				case "cancel-revision", "cancel-finish":
					_, err = s.db.Exec(`DROP TRIGGER swarm_child_tasks_fence`)
					mustNil(t, err)
					if change == "cancel-revision" {
						_, err = s.db.Exec(`UPDATE swarm_child_tasks SET revision=3 WHERE id=?`, child.ID)
					} else {
						_, err = s.db.Exec(`UPDATE swarm_child_tasks SET finished_at='' WHERE id=?`, child.ID)
					}
					mustNil(t, err)
				case "unknown-stage":
					setActor(t, s, a.UID, "operation='start',stage='start',phase='provisioning',upid='',error=''")
				case "physical":
					setActor(t, s, a.UID, "owner_nonce='revoked'")
				case "manual":
					if phase == "begin" {
						_, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "manual-delete", "delete")
						mustNil(t, err)
					} else {
						setActor(t, s, a.UID, "operation_id='manual-delete'")
					}
				case "workspace", "parent-workspace":
					uid := a.UID
					if change == "parent-workspace" {
						uid = job.Parent.Principal.ActorUID
					}
					op := ActorOperation{ProjectID: job.Parent.Principal.ProjectID, SwarmID: job.Parent.Principal.SwarmID, UID: uid, Generation: 1, OperationID: "unknown-workspace", Caller: "trusted", Operation: "workspace_attest", State: "unconfirmed"}
					_, err = s.db.Exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, encodeManaged(op), uid)
					mustNil(t, err)
				case "coordination":
					claim := CoordinationClaim{ProjectID: job.Parent.Principal.ProjectID, SwarmID: job.Parent.Principal.SwarmID, Source: CoordinationActor{UID: a.UID, Generation: 1}, RequestID: "held"}
					_, err = s.db.Exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'landing',?,?,?)`, claim.ProjectID, claim.RequestID, encodeManaged(claim), "held-slot")
					mustNil(t, err)
				case "intervention", "parent-intervention":
					uid := a.UID
					if change == "parent-intervention" {
						uid = job.Parent.Principal.ActorUID
					}
					_, err = s.db.Exec(`INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES('manual',?,?,?,1,'u','human','session_terminal','completed','now')`, job.Parent.Principal.ProjectID, job.Parent.Principal.SwarmID, uid)
					mustNil(t, err)
				}
				before := databaseRows(t, s.db)
				switch phase {
				case "begin":
					_, err = s.BeginEphemeralChildCleanup(ctx, job.ID, auth)
				case "check":
					err = s.CheckEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID, auth)
				case "reserve":
					_, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, auth)
				}
				if err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("revoked scope authorized or changed state")
				}
			})
		}
	}
}

func TestEphemeralCancelCleanupAtomicNonnested(t *testing.T) {
	for _, stage := range []string{"begin", "reserve"} {
		for _, change := range []string{"insert-ignore", "update-ignore", "abort", "normal-nested", "full-nested"} {
			if stage == "reserve" && (change == "insert-ignore" || change == "abort") {
				continue
			}
			t.Run(stage+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				s, job, _ := cancelledCleanupFixture(t, true)
				var a ManagedActor
				var err error
				if stage == "reserve" {
					a, err = s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
					mustNil(t, err)
				}
				switch change {
				case "insert-ignore":
					_, err = s.db.Exec(`CREATE TRIGGER injected BEFORE INSERT ON ephemeral_child_lifecycles BEGIN SELECT RAISE(IGNORE); END`)
				case "update-ignore":
					_, err = s.db.Exec(`CREATE TRIGGER injected BEFORE UPDATE ON managed_actors BEGIN SELECT RAISE(IGNORE); END`)
				case "abort":
					_, err = s.db.Exec(`CREATE TRIGGER injected AFTER INSERT ON ephemeral_child_lifecycles BEGIN SELECT RAISE(ABORT,'injected'); END`)
				}
				mustNil(t, err)
				run := func(ctx context.Context) error {
					if stage == "begin" {
						_, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
						return err
					}
					_, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, childAllow)
					return err
				}
				before := databaseRows(t, s.db)
				if strings.HasSuffix(change, "nested") {
					tx := s.WithTx
					if change == "full-nested" {
						tx = s.WithDurableTx
					}
					mustNil(t, tx(ctx, func(ctx context.Context) error {
						if err := run(ctx); !errors.Is(err, ErrDurableTransactionNested) {
							t.Fatalf("nested: %v", err)
						}
						return nil
					}))
				} else if err := run(ctx); err == nil {
					t.Fatal("ignored/aborted write succeeded")
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("partial write persisted")
				}
			})
		}
	}
}

func TestEphemeralCancelCleanupReopenAndConcurrentRetry(t *testing.T) {
	ctx := context.Background()
	s, job, _ := cancelledCleanupFixture(t, true)
	var wg sync.WaitGroup
	results := make(chan ManagedActor, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { a, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); results <- a; errs <- err })
	}
	wg.Wait()
	a, b := <-results, <-results
	mustNil(t, <-errs)
	mustNil(t, <-errs)
	if a != b {
		t.Fatal("concurrent retry minted operations")
	}
	a, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, childAllow)
	mustNil(t, err)
	var path string
	mustNil(t, s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	before := databaseRows(t, reopened.db)
	retry, err := reopened.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
	mustNil(t, err)
	mustNil(t, reopened.CheckEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID, childAllow))
	if retry != a {
		t.Fatal("reopen changed operation")
	}
	if _, err := reopened.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, childAllow); err == nil {
		t.Fatal("reopen rearmed unknown stop")
	}
	if !reflect.DeepEqual(before, databaseRows(t, reopened.db)) {
		t.Fatal("reopen retry changed evidence")
	}
}

func TestEphemeralCancelCleanupSettledMixedJoin(t *testing.T) {
	for _, mode := range []string{"all", "any", "quorum"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			q := 0
			if mode == "quorum" {
				q = 1
			}
			s, parent, executor := childFixture(t, mode, 2, q)
			job, err := s.EnqueueEphemeralChild(ctx, parent, SwarmChildJoinPolicy{Mode: mode, Total: 2, Quorum: q, Remaining: "retain"}, "cancelled", "Unsent isolated task", "pool", childAllow)
			mustNil(t, err)
			addEphemeralCapacity(t, s)
			allocation, err := s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
			mustNil(t, err)
			setActor(t, s, allocation.ActorUID, "phase='running',stage='done',operation='start'")
			attestForRegistration(t, s, job)
			child, err := s.RegisterEphemeralChild(ctx, job.ID, childAllow)
			mustNil(t, err)
			_, err = s.CancelSwarmChild(ctx, parent, child.RequestID, child.Revision, childAllow)
			mustNil(t, err)
			// Отсутствующее обязательство также считается open; cancellation не early cleanup.
			before := databaseRows(t, s.db)
			if _, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("open aggregate authorized cleanup")
			}
			finishChild(t, s, parent, executor, "success", "success")
			before = databaseRows(t, s.db)
			if _, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("computed but unapplied aggregate authorized cleanup")
			}
			join, err := s.JoinSwarmChildren(ctx, parent, childAllow)
			mustNil(t, err)
			want := "ready_success"
			if mode == "all" {
				want = "ready_fail"
			}
			if join.Evaluation.State != want || join.Evaluation.Open != 0 {
				t.Fatalf("wrong join: %+v", join)
			}
			a, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
			mustNil(t, err)
			h, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID)
			mustNil(t, err)
			if h.Kind != "cancel_cleanup" {
				t.Fatal("not cancellation provenance")
			}
		})
	}
}

func TestEphemeralCancelCleanupReadyAggregateStillHoldsOpenMember(t *testing.T) {
	ctx := context.Background()
	s, parent, executor := childFixture(t, "any", 3, 0)
	job, err := s.EnqueueEphemeralChild(ctx, parent, SwarmChildJoinPolicy{Mode: "any", Total: 3, Remaining: "retain"}, "cancelled", "Unsent isolated task", "pool", childAllow)
	mustNil(t, err)
	addEphemeralCapacity(t, s)
	_, err = s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
	attestForRegistration(t, s, job)
	child, err := s.RegisterEphemeralChild(ctx, job.ID, childAllow)
	mustNil(t, err)
	_, err = s.CancelSwarmChild(ctx, parent, child.RequestID, child.Revision, childAllow)
	mustNil(t, err)
	finishChild(t, s, parent, executor, "success", "success")
	last, err := s.ReserveSwarmChild(ctx, parent, "last", "Still outstanding", executor, childAllow)
	mustNil(t, err)
	join, err := s.loadChildJoin(ctx, parent)
	mustNil(t, err)
	if join.Evaluation.State != "ready_success" || join.Evaluation.Open != 1 {
		t.Fatalf("not ready/open: %+v", join)
	}
	before := databaseRows(t, s.db)
	observed, err := s.JoinSwarmChildren(ctx, parent, childAllow)
	mustNil(t, err)
	visit, err := s.GetSwarmStep(ctx, parent.Principal.SwarmID, parent.Sequence)
	mustNil(t, err)
	if observed.Evaluation != join.Evaluation || visit.State == "completed" || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("nonblocking readiness observation applied an open join")
	}
	if _, err = s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ready-success aggregate released an unsettled obligation")
	}
	_, err = s.CancelSwarmChild(ctx, parent, last.RequestID, last.Revision, childAllow)
	mustNil(t, err)
	_, err = s.JoinSwarmChildren(ctx, parent, childAllow)
	mustNil(t, err)
	_, err = s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
	mustNil(t, err)
}

func TestEphemeralCancelCleanupLedgerImmutableAndExclusive(t *testing.T) {
	ctx := context.Background()
	s, job, _ := cancelledCleanupFixture(t, true)
	actor, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	for _, kind := range []string{"cleanup", "failure_cleanup", "cancel_cleanup"} {
		if _, err := s.db.Exec(`INSERT INTO ephemeral_child_lifecycles(intent_id,kind,operation_id,created_at) VALUES(?,?,?,'now')`, job.ID, kind, actor.OperationID); err == nil {
			t.Fatal("second cleanup provenance accepted")
		}
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO ephemeral_child_lifecycles(intent_id,kind,operation_id,created_at) VALUES(?,?,?,'now')`, job.ID, kind, actor.OperationID); err == nil {
			t.Fatal("cleanup provenance replaced")
		}
	}
	for _, query := range []string{`UPDATE ephemeral_child_lifecycles SET kind='cleanup'`, `DELETE FROM ephemeral_child_lifecycles`} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatal("lifecycle history mutable")
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ledger refusal changed history")
	}
}

func TestEphemeralCancelCleanupRefusesPreparedAndUnknownTransport(t *testing.T) {
	for _, transport := range []string{"prepared", "unknown", "legacy"} {
		t.Run(transport, func(t *testing.T) {
			ctx := context.Background()
			s, job, child, in := childDeliveryFixture(t)
			if transport != "legacy" {
				d, err := s.PrepareEphemeralChildDelivery(ctx, job.ID, in, childAllow)
				mustNil(t, err)
				if transport == "unknown" {
					now := time.Now().UTC()
					_, err = s.ReserveActorDeliveryAttempt(ctx, d.ID, now, now.Add(time.Minute), allow)
					mustNil(t, err)
				}
				before := databaseRows(t, s.db)
				if _, err := s.CancelSwarmChild(ctx, job.Parent, child.RequestID, 2, childAllow); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("typed Prepare allowed cancellation")
				}
			}
			// Симулируем неоднозначный старый snapshot: даже revision-2 cancellation
			// не считается no-execution, если существует instruction/legacy transport.
			_, err := s.db.Exec(`DROP TRIGGER swarm_child_tasks_fence; UPDATE swarm_child_tasks SET state='cancelled',revision=2,finished_at='now' WHERE id=?`, child.ID)
			mustNil(t, err)
			if transport == "legacy" {
				_, err = s.db.Exec(`UPDATE swarm_child_tasks SET role='legacy',delivery='unconfirmed',delivery_detail='issued legacy transport' WHERE id=?`, child.ID)
				mustNil(t, err)
			}
			_, err = s.JoinSwarmChildren(ctx, job.Parent, childAllow)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			if _, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("ambiguous transport gained no-execution provenance")
			}
		})
	}
}

func TestEphemeralCancelCleanupDoesNotAdoptOrdinaryChild(t *testing.T) {
	ctx := context.Background()
	s, parent, executor := childFixture(t, "all", 1, 0)
	child, err := s.ReserveSwarmChild(ctx, parent, "ordinary", "Persistent/native work", executor, childAllow)
	mustNil(t, err)
	_, err = s.CancelSwarmChild(ctx, parent, child.RequestID, child.Revision, childAllow)
	mustNil(t, err)
	_, err = s.JoinSwarmChildren(ctx, parent, childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	if _, err := s.BeginEphemeralChildCleanup(ctx, child.ID, childAllow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary child adopted: %v", err)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ordinary child changed")
	}
}

func TestEphemeralCancelCleanupMigrationPreserves60(t *testing.T) {
	path := loadSchemaFixture(t, 60)
	db := rawDatabase(t, path)
	before := withEmptyPolicyReviewTables(databaseRows(t, db))
	mustNil(t, db.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("60→61 changed historical rows")
	}
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion {
		t.Fatalf("version=%d", version)
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM ephemeral_child_lifecycles WHERE kind='cancel_cleanup'`).Scan(&count))
	if count != 0 {
		t.Fatal("migration inferred historical cancellation provenance")
	}
}

func TestEphemeralCancelCleanupAppliedJoin(t *testing.T) {
	ctx := context.Background()
	s, job, child := cancelledCleanupFixture(t, false)
	before := databaseRows(t, s.db)
	if _, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("unapplied cancellation join authorized cleanup or changed state")
	}
	_, err := s.JoinSwarmChildren(ctx, job.Parent, childAllow)
	mustNil(t, err)
	before = databaseRows(t, s.db)
	actor, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
	if err != nil {
		t.Fatalf("registered pending cancellation after applied join cannot begin cleanup: %v", err)
	}
	h, err := s.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
	mustNil(t, err)
	if h.Kind != "cancel_cleanup" || actor.Operation != "delete" || actor.Stage != "stop" || actor.Unsent() != ManagedStopUnsent || actor.UPID != "" || actor.DeletedAt != nil {
		t.Fatalf("not distinct unsent cleanup: %+v %+v", h, actor)
	}
	after := databaseRows(t, s.db)
	for _, table := range []string{"managed_actors", "ephemeral_child_lifecycles"} {
		delete(before, table)
		delete(after, table)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("cleanup changed cancellation/report/delivery/proof/physical grants or capacity")
	}
	join, err := s.loadChildJoin(ctx, job.Parent)
	mustNil(t, err)
	if len(join.Children) != 1 || join.Children[0] != child {
		t.Fatal("cancellation evidence changed")
	}
}
