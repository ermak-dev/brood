package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Cleanup после явного server failure и применённого failed parent join.
// SPEC: Не seal/report, не provider receipt; unknown stages и workspace holds остаются.
// Только exact queued original Create допускает атомарную замену без terminal Create.
// IMPLEMENTATION: Distinct failure_cleanup в существующем ledger, FULL begin/stage CAS.
// RESULT: IMPLEMENTED — behavioral RED на seal-only пути до реализации.
// STATUS: DONE
func failureCleanupFixture(t *testing.T, state string, joined bool) (*Store, EphemeralChildIntent) {
	t.Helper()
	ctx := context.Background()
	var s *Store
	var job EphemeralChildIntent
	switch state {
	case "registered":
		s, job, _ = registeredFailureFixture(t)
		mustNil(t, s.RecordEphemeralChildExecutorFailure(ctx, job.ID, "Explicit executor rejection", childAllow))
	case "queued":
		s, job = createGrantFixture(t)
		mustNil(t, s.RecordEphemeralChildFailure(ctx, job.ID, "Explicit bootstrap rejection", childAllow))
	case "terminal":
		s, job = lifecycleFixture(t, "start")
		mustNil(t, s.RecordEphemeralChildFailure(ctx, job.ID, "Explicit bootstrap rejection", childAllow))
	case "unallocated":
		s, job = allocationFixture(t, 1)
		mustNil(t, s.RecordEphemeralChildFailure(ctx, job.ID, "Explicit admission rejection", childAllow))
	default:
		t.Fatal(state)
	}
	if joined {
		_, err := s.JoinSwarmChildren(ctx, job.Parent, childAllow)
		mustNil(t, err)
	}
	return s, job
}

func TestEphemeralFailureCleanupAppliedJoinAndStages(t *testing.T) {
	for _, state := range []string{"queued", "terminal", "registered"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s, job := failureCleanupFixture(t, state, false)
			before := databaseRows(t, s.db)
			if _, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); err == nil {
				t.Fatal("unapplied failure join authorized cleanup")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("denied begin wrote state")
			}
			_, err := s.JoinSwarmChildren(ctx, job.Parent, childAllow)
			mustNil(t, err)
			stable := databaseRows(t, s.db)
			calls := 0
			action := "begin_ephemeral_cleanup"
			auth := func(ctx context.Context, a SwarmChildAuthorization) error {
				calls++
				var sync int
				mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync))
				if a.Action != action || a.Parent != job.Parent || a.Caller != job.Parent.Principal || a.RequestID != job.RequestID || a.Payload != job.Payload || a.Executor.ActorUID != "isolated" || a.Executor.Incarnation != 1 || !sameChildScope(a.Executor, job.Parent.Principal) {
					t.Fatalf("wrong cleanup scope: %+v", a)
				}
				if action != "check_ephemeral_cleanup" && sync != 2 {
					t.Fatal("not FULL")
				}
				return nil
			}
			a, err := s.BeginEphemeralChildCleanup(ctx, job.ID, auth)
			mustNil(t, err)
			if a.Operation != "delete" || a.Stage != "stop" || a.Desired != "deleted" || a.Unsent() != ManagedStopUnsent || a.UPID != "" {
				t.Fatalf("bad cleanup: %+v", a)
			}
			h, err := s.GetEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID)
			mustNil(t, err)
			if h.Kind != "failure_cleanup" {
				t.Fatalf("failure source not distinct: %+v", h)
			}
			after := databaseRows(t, s.db)
			for _, table := range []string{"managed_actors", "ephemeral_child_lifecycles"} {
				delete(stable, table)
				delete(after, table)
			}
			if !reflect.DeepEqual(stable, after) {
				t.Fatal("cleanup changed reason/report/transport/seal/physical claims")
			}
			stable = databaseRows(t, s.db)
			retry, err := s.BeginEphemeralChildCleanup(ctx, job.ID, auth)
			mustNil(t, err)
			if retry != a || calls != 2 || !reflect.DeepEqual(stable, databaseRows(t, s.db)) {
				t.Fatal("retry minted work or skipped authority")
			}
			action = "check_ephemeral_cleanup"
			mustNil(t, s.CheckEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID, auth))
			if _, err := s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID); err == nil {
				t.Fatal("failure bypassed stop/absence proof")
			}
			action = "reserve_ephemeral_cleanup_stop"
			requested, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", a.Revision, auth)
			mustNil(t, err)
			if _, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "stop", requested.Revision, auth); err == nil {
				t.Fatal("unknown stop replayed")
			}
			requested, err = s.BindManagedActorTask(ctx, a.UID, requested.Revision, a.OperationID, "stop", "positive-stop-task")
			mustNil(t, err)
			requested, err = s.ConfirmManagedActorStage(ctx, a.UID, requested.Revision, a.OperationID, "stop", "stopped")
			mustNil(t, err)
			action = "reserve_ephemeral_cleanup_delete"
			requested, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "delete", requested.Revision, auth)
			mustNil(t, err)
			if _, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, "delete", requested.Revision, auth); err == nil {
				t.Fatal("unknown delete replayed")
			}
			action = "begin_ephemeral_cleanup"
			if _, err := s.BeginEphemeralChildCleanup(ctx, job.ID, auth); err != nil {
				t.Fatal("exact begin retry should only return existing unknown operation:", err)
			}
		})
	}
}

func TestEphemeralFailureCleanupHoldsAndRevocation(t *testing.T) {
	for _, kind := range []string{"no-failure", "unallocated", "nil", "denied", "source", "parent", "incarnation", "physical", "workspace", "intervention", "reserved-create", "create-task", "queued-create-task", "unknown-start", "unknown-stop", "unknown-delete"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			state := "terminal"
			if kind == "unallocated" {
				state = "unallocated"
			}
			if kind == "queued-create-task" {
				state = "queued"
			}
			if kind == "reserved-create" || kind == "create-task" {
				state = "queued"
			}
			var s *Store
			var job EphemeralChildIntent
			if kind == "no-failure" {
				s, job = lifecycleFixture(t, "start")
			} else if kind == "reserved-create" || kind == "create-task" {
				s, job = createGrantFixture(t)
				_, err := s.ReserveEphemeralChildCreate(ctx, job.ID, childAllow)
				mustNil(t, err)
				if kind == "create-task" {
					a, err := s.GetManagedActor(ctx, "isolated")
					mustNil(t, err)
					_, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, "create", "unknown-create-task")
					mustNil(t, err)
				}
				mustNil(t, s.RecordEphemeralChildFailure(ctx, job.ID, "Explicit rejection is not task reconciliation", childAllow))
				_, err = s.JoinSwarmChildren(ctx, job.Parent, childAllow)
				mustNil(t, err)
			} else {
				s, job = failureCleanupFixture(t, state, true)
			}
			auth := SwarmChildAuthorize(childAllow)
			switch kind {
			case "queued-create-task":
				a, err := s.GetManagedActor(ctx, "isolated")
				mustNil(t, err)
				_, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, "create", "late-task")
				mustNil(t, err)
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "source":
				_, err := s.db.Exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://other.example/repo.git' WHERE swarm_id='one'`)
				mustNil(t, err)
			case "parent":
				setActor(t, s, job.Parent.Principal.ActorUID, "phase='stopped'")
			case "incarnation":
				setActor(t, s, "isolated", "incarnation=2")
			case "physical":
				setActor(t, s, "isolated", "owner_nonce='wrong'")
			case "workspace":
				op := ActorOperation{ProjectID: job.Parent.Principal.ProjectID, SwarmID: job.Parent.Principal.SwarmID, UID: "isolated", Generation: 1, OperationID: "unknown-workspace", Caller: "trusted", Operation: "workspace_attest", State: "unconfirmed"}
				_, err := s.db.Exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, encodeManaged(op), op.UID)
				mustNil(t, err)
			case "intervention":
				_, err := s.db.Exec(`INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES('intervention',?,?,'isolated',1,'u','human','session_terminal','completed','now')`, job.Parent.Principal.ProjectID, job.Parent.Principal.SwarmID)
				mustNil(t, err)
			case "unknown-start", "unknown-stop", "unknown-delete":
				op := kind[len("unknown-"):]
				setActor(t, s, "isolated", "operation='"+op+"',stage='"+op+"',upid='',phase='deleting',error=''")
			}
			before := databaseRows(t, s.db)
			if _, err := s.BeginEphemeralChildCleanup(ctx, job.ID, auth); err == nil {
				t.Fatal("unsafe cleanup authorized")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refusal changed held state")
			}
		})
	}
}

func TestEphemeralFailureCleanupAtomicAndNonnested(t *testing.T) {
	for _, kind := range []string{"insert-ignore", "update-ignore", "abort", "normal-nested", "full-nested"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, job := failureCleanupFixture(t, "queued", true)
			switch kind {
			case "insert-ignore":
				_, err := s.db.Exec(`CREATE TRIGGER injected BEFORE INSERT ON ephemeral_child_lifecycles BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			case "update-ignore":
				_, err := s.db.Exec(`CREATE TRIGGER injected BEFORE UPDATE ON managed_actors BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			case "abort":
				_, err := s.db.Exec(`CREATE TRIGGER injected AFTER INSERT ON ephemeral_child_lifecycles BEGIN SELECT RAISE(ABORT,'injected'); END`)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			if kind == "normal-nested" || kind == "full-nested" {
				run := s.WithTx
				if kind == "full-nested" {
					run = s.WithDurableTx
				}
				mustNil(t, run(ctx, func(ctx context.Context) error {
					_, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
					if !errors.Is(err, ErrDurableTransactionNested) {
						t.Fatalf("nested: %v", err)
					}
					return nil
				}))
			} else {
				if _, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow); err == nil {
					t.Fatal("ignored write succeeded")
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("partial operation persisted")
			}
		})
	}
}

func TestEphemeralFailureCleanupOfficial58Fixture(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 58))
	defer db.Close()
	var n int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM ephemeral_child_lifecycles l JOIN ephemeral_child_executor_failures f ON f.intent_id=l.intent_id JOIN swarm_child_tasks c ON c.id=f.child_id WHERE l.kind='failure_cleanup' AND c.state='pending' AND coalesce(c.report,'')='' AND c.delivery=''`).Scan(&n))
	if n != 1 {
		t.Fatal("official fixture lacks real failure cleanup without synthetic child report")
	}
}

func TestEphemeralFailureCleanupMigrationPreserves57(t *testing.T) {
	path := loadSchemaFixture(t, 57)
	db := rawDatabase(t, path)
	before := withEmptyPolicyReviewTables(databaseRows(t, db))
	before["swarm_eval_bindings"] = nil
	before["swarm_eval_replays"] = nil
	mustNil(t, db.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("migration changed existing rows or classified old cleanup as failure")
	}
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion {
		t.Fatalf("schema = %d, want %d", version, schemaVersion)
	}
}
