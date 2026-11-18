package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Серверный отказ уже зарегистрированного executor без выдуманного actor report.
// SPEC: Один слот, FULL nonnested commit, mandatory authority, immutable reason,
// broken executor допустим, unknown workspace/owner intervention запрещены; physical holds неизменны.
// IMPLEMENTATION: Отдельный immutable ledger и typed ExecutorFailures, без изменения task outcome.
// RESULT: IMPLEMENTED — behavioral RED до API/schema57; summary и registration retry также RED→GREEN.
// STATUS: DONE
func recordExecutorFailure(t *testing.T, s *Store, ctx context.Context, id, reason string, auth SwarmChildAuthorize) error {
	t.Helper()
	return s.RecordEphemeralChildExecutorFailure(ctx, id, reason, auth)
}

func registeredFailureFixture(t *testing.T) (*Store, EphemeralChildIntent, SwarmChildTask) {
	t.Helper()
	s, job := ephemeralAttestationFixture(t, true)
	attestForRegistration(t, s, job)
	c, err := s.RegisterEphemeralChild(context.Background(), job.ID, childAllow)
	mustNil(t, err)
	return s, job, c
}

func TestEphemeralExecutorFailureOneSlotAndHonestJoin(t *testing.T) {
	ctx := context.Background()
	s, job, c := registeredFailureFixture(t)
	setActor(t, s, c.Executor.ActorUID, "phase='stopped'")
	before := databaseRows(t, s.db)
	calls := 0
	auth := func(ctx context.Context, a SwarmChildAuthorization) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if mode != 2 || a.Action != "fail_ephemeral_executor" || a.Parent != job.Parent || a.Executor != c.Executor || a.RequestID != job.RequestID || a.Payload != job.Payload || a.Caller != job.Parent.Principal {
			t.Fatalf("wrong authorization: %+v sync=%d", a, mode)
		}
		return nil
	}
	reason := "Executor startup rejected by explicit local validation."
	mustNil(t, recordExecutorFailure(t, s, ctx, job.ID, reason, auth))
	after := databaseRows(t, s.db)
	delete(before, "ephemeral_child_executor_failures")
	delete(after, "ephemeral_child_executor_failures")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("server failure rewrote actor/transport/proof/physical state")
	}
	j, err := s.GetSwarmChildJoin(ctx, job.Parent, childAllow)
	mustNil(t, err)
	if len(j.Children) != 1 || j.Children[0] != c || j.Evaluation != (SwarmChildJoinEvaluation{State: "ready_fail", Open: 0}) {
		t.Fatalf("failure must settle exactly one registered slot: %+v", j)
	}
	if !strings.Contains(ChildJoinReport(j), reason) || !strings.Contains(ChildJoinReport(j), "server executor failure") {
		t.Fatalf("missing distinct server reason: %s", ChildJoinReport(j))
	}
	stable := databaseRows(t, s.db)
	mustNil(t, recordExecutorFailure(t, s, ctx, job.ID, reason, auth))
	if calls != 2 || !reflect.DeepEqual(stable, databaseRows(t, s.db)) {
		t.Fatal("retry changed fact or skipped authorization")
	}
	if !errors.Is(recordExecutorFailure(t, s, ctx, job.ID, reason+" changed", childAllow), ErrSwarmChildConflict) {
		t.Fatal("changed reason accepted")
	}
	page, err := s.SwarmChildGroups(ctx, job.Parent.Principal.SwarmID, 0, 10)
	mustNil(t, err)
	if page.Items[0].Registered != 1 || page.Items[0].Open != 0 {
		t.Fatalf("summary double counted: %+v", page)
	}
	if page.Items[0].ExecutorFailed != 1 {
		t.Fatal("summary lacks the separate registered server failure count")
	}
	if err := s.SealEphemeralChildResult(ctx, job.ID, childAllow); err == nil {
		t.Fatal("failure became actor result seal")
	}
	joined, err := s.JoinSwarmChildren(ctx, job.Parent, childAllow)
	mustNil(t, err)
	if joined.Evaluation.State != "ready_fail" {
		t.Fatal("join reversed failure")
	}
	a, err := s.BeginEphemeralChildCleanup(ctx, job.ID, childAllow)
	mustNil(t, err)
	h, err := s.GetEphemeralChildLifecycle(ctx, a.UID, a.Incarnation, a.OperationID)
	mustNil(t, err)
	if h.Kind != "failure_cleanup" {
		t.Fatal("applied failure join did not retain distinct cleanup source")
	}
	if _, err := s.GetEphemeralChildResultSeal(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("failure cleanup invented seal")
	}
}

// Историческое вмешательство parent до текущего шага не является текущим hold.
func TestEphemeralExecutorFailureDistinguishesHistoricalParentControl(t *testing.T) {
	ctx := context.Background()
	for _, current := range []bool{false, true} {
		t.Run(map[bool]string{false: "historical", true: "current"}[current], func(t *testing.T) {
			s, job, _ := registeredFailureFixture(t)
			parent, err := s.GetManagedActor(ctx, job.Parent.Principal.ActorUID)
			mustNil(t, err)
			sw, err := s.GetSwarm(ctx, parent.SwarmID)
			mustNil(t, err)
			if current {
				op, err := s.AcquireOwnerActorOperation(ctx, parent, "parent-control", "operator:"+sw.OwnerID, "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				mustNil(t, err)
				mustNil(t, s.FinishActorOperation(ctx, op, true))
			} else {
				_, err = s.db.Exec(`INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES('parent-control',?,?,?,?,?,?,'session_terminal','completed',?)`, sw.ProjectID, sw.ID, parent.UID, parent.Incarnation, sw.OwnerID, "operator:"+sw.OwnerID, parent.CreatedAt)
				mustNil(t, err)
			}
			err = s.RecordEphemeralChildExecutorFailure(ctx, job.ID, "Explicit executor failure", childAllow)
			if current && err == nil {
				t.Fatal("current interrupted parent granted automatic failure authority")
			}
			if !current && err != nil {
				t.Fatalf("unrelated historical parent control permanently disabled failure accounting: %v", err)
			}
		})
	}
}

func TestEphemeralExecutorFailureAuthorityAndAtomicity(t *testing.T) {
	for _, kind := range []string{"nil", "denied", "ignored", "nested", "durable-nested", "blank", "oversize", "incarnation", "parent", "source", "owner", "owner-settled", "unknown-workspace"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, job, c := registeredFailureFixture(t)
			auth := SwarmChildAuthorize(childAllow)
			reason := "Explicit local failure"
			switch kind {
			case "nil":
				auth = nil
			case "denied":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "ignored":
				_, err := s.db.Exec(`CREATE TRIGGER ignore_executor_failure BEFORE INSERT ON ephemeral_child_executor_failures BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			case "blank":
				reason = " \n"
			case "oversize":
				reason = strings.Repeat("x", 1025)
			case "incarnation":
				setActor(t, s, c.Executor.ActorUID, "incarnation=2")
			case "parent":
				setActor(t, s, job.Parent.Principal.ActorUID, "incarnation=2")
			case "source":
				_, err := s.db.Exec(`DROP TRIGGER pipeline_sources_immutable; UPDATE pipeline_sources SET origin='https://other.example/repo.git' WHERE swarm_id='one'`)
				mustNil(t, err)
			case "owner", "owner-settled":
				a, err := s.GetManagedActor(ctx, c.Executor.ActorUID)
				mustNil(t, err)
				sw, err := s.GetSwarm(ctx, a.SwarmID)
				mustNil(t, err)
				op, err := s.AcquireOwnerActorOperation(ctx, a, "failure-owner", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
				mustNil(t, err)
				if kind == "owner-settled" {
					mustNil(t, s.FinishActorOperation(ctx, op, true))
				}
			case "unknown-workspace":
				op, err := s.acquireActorOperation(ctx, c.Executor.ActorUID, 1, "unknown", "trusted", "workspace_attest", func(context.Context, ManagedActor) error { return nil })
				mustNil(t, err)
				mustNil(t, s.FinishActorOperation(ctx, op, false))
			}
			before := databaseRows(t, s.db)
			var err error
			if kind == "nested" || kind == "durable-nested" {
				run := s.WithTx
				if kind == "durable-nested" {
					run = s.WithDurableTx
				}
				mustNil(t, run(ctx, func(ctx context.Context) error {
					err = recordExecutorFailure(t, s, ctx, job.ID, reason, auth)
					return nil
				}))
			} else {
				err = recordExecutorFailure(t, s, ctx, job.ID, reason, auth)
			}
			if err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatalf("%s accepted or wrote state: %v", kind, err)
			}
		})
	}
}

func TestEphemeralExecutorFailureMigrationPreserves56(t *testing.T) {
	path := loadSchemaFixture(t, 56)
	raw := rawDatabase(t, path)
	before := withEmptyPolicyReviewTables(databaseRows(t, raw))
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	var version int
	mustNil(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	if version != schemaVersion {
		t.Fatalf("schema = %d, want %d", version, schemaVersion)
	}
	before["ephemeral_child_executor_failures"] = nil
	before["swarm_eval_bindings"] = nil
	before["swarm_eval_replays"] = nil
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("migration changed old rows or inferred executor failure")
	}
	requireIntact(t, s.db)
}
