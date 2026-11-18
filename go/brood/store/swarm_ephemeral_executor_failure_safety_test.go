package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// FEATURE: Серверный отказ registered child не подменяет отчёт или physical permission.
// SPEC: Смешанные all/any/quorum учитывают каждый слот один раз; поздний отчёт
// и повторная доставка не отменяют отказ.
// BUG: Подготовленная до failure инструкция сохраняла fresh send authority.
// HYPOTHESIS: Transport current-source gate обходил проверку executor failure.
// SOLUTION: Current gate и candidates исключают retained executor failure.
// RESULT: FIXED — current/reserve/candidates RED→GREEN, transport history неизменна.
// STATUS: FIXED
func TestEphemeralExecutorFailureFencesPreparedTransport(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{"current", "reserve", "candidates"} {
		t.Run(action, func(t *testing.T) {
			s, job, _, in := childDeliveryFixture(t)
			delivery, err := s.PrepareEphemeralChildDelivery(ctx, job.ID, in, childAllow)
			mustNil(t, err)
			mustNil(t, s.CheckActorDeliveryCurrent(ctx, delivery.ID))
			candidates, err := s.PendingActorDeliveryChildren(ctx, "", 64)
			mustNil(t, err)
			if len(candidates) != 1 || candidates[0].ID != delivery.ID {
				t.Fatal("fixture did not prepare a dispatchable instruction")
			}
			mustNil(t, s.RecordEphemeralChildExecutorFailure(ctx, job.ID, "Explicit executor rejection before send", childAllow))
			before := databaseRows(t, s.db)
			switch action {
			case "current":
				if err := s.CheckActorDeliveryCurrent(ctx, delivery.ID); err == nil {
					t.Fatal("prepared instruction remained current after executor failure")
				}
			case "reserve":
				now := time.Now()
				if ticket, err := s.ReserveActorDeliveryAttempt(ctx, delivery.ID, now, now.Add(time.Minute), allow); err == nil {
					t.Fatalf("server-failed executor received fresh transport permission: %+v", ticket)
				}
			case "candidates":
				candidates, err := s.PendingActorDeliveryChildren(ctx, "", 64)
				mustNil(t, err)
				if len(candidates) != 0 {
					t.Fatalf("server-failed instruction remained a dispatch candidate: %+v", candidates)
				}
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("failure fence changed retained delivery history")
			}
		})
	}
}

func TestEphemeralExecutorFailureCannotRenewDelivery(t *testing.T) {
	ctx := context.Background()
	s, job, _, in := childDeliveryFixture(t)
	mustNil(t, recordExecutorFailure(t, s, ctx, job.ID, "Explicit local rejection", childAllow))
	before := databaseRows(t, s.db)
	if _, err := s.RegisterEphemeralChild(ctx, job.ID, childAllow); err == nil {
		t.Fatal("server-failed job regained registration authority")
	}
	if _, err := s.PrepareEphemeralChildDelivery(ctx, job.ID, in, childAllow); err == nil {
		t.Fatal("server-failed child acquired a fresh delivery intent")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("failure refusal changed rows")
	}
}

func TestEphemeralExecutorFailureMixedReports(t *testing.T) {
	for _, tc := range []struct {
		mode              string
		quorum, successes int
		want              string
	}{
		{"all", 0, 2, "ready_fail"}, {"any", 0, 1, "ready_success"}, {"any", 0, 0, "ready_fail"}, {"quorum", 2, 2, "ready_success"}, {"quorum", 2, 1, "ready_fail"},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.mode, tc.successes), func(t *testing.T) {
			ctx := context.Background()
			s, p, e := childFixture(t, tc.mode, 3, tc.quorum)
			job, err := s.EnqueueEphemeralChild(ctx, p, SwarmChildJoinPolicy{Mode: tc.mode, Total: 3, Quorum: tc.quorum, Remaining: "retain"}, "registered-failure", "Isolated task", "pool", childAllow)
			mustNil(t, err)
			addEphemeralCapacity(t, s)
			_, err = s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
			mustNil(t, err)
			setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
			attestForRegistration(t, s, job)
			c, err := s.RegisterEphemeralChild(ctx, job.ID, childAllow)
			mustNil(t, err)
			mustNil(t, recordExecutorFailure(t, s, ctx, job.ID, "Local validation failed", childAllow))
			j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			if j.Evaluation != (SwarmChildJoinEvaluation{State: "pending", Open: 2}) {
				t.Fatalf("missing slots not held: %+v", j)
			}
			for n := 0; n < 2; n++ {
				real := reserveChild(t, s, p, e, fmt.Sprintf("real-%d", n))
				real, err = s.StartSwarmChild(ctx, p, real.RequestID, e, real.Revision, childAllow)
				mustNil(t, err)
				outcome := "fail"
				if n < tc.successes {
					outcome = "success"
				}
				_, err = s.CompleteSwarmChild(ctx, p, real.RequestID, e, real.Revision, outcome, "Independent report", childAllow)
				mustNil(t, err)
			}
			j, err = s.JoinSwarmChildren(ctx, p, childAllow)
			mustNil(t, err)
			if len(j.Children) != 3 || len(j.ExecutorFailures) != 1 || j.ExecutorFailures[0].ChildID != c.ID || j.Evaluation != (SwarmChildJoinEvaluation{State: tc.want, Open: 0}) {
				t.Fatalf("wrong mixed join: %+v", j)
			}
			page, err := s.SwarmChildGroups(ctx, p.Principal.SwarmID, 0, 10)
			mustNil(t, err)
			if page.Items[0].Registered != 3 || page.Items[0].Open != 0 {
				t.Fatalf("summary counted fact as child: %+v", page)
			}
		})
	}
}

func TestEphemeralExecutorFailureLateReportAndPhysicalHolds(t *testing.T) {
	ctx := context.Background()
	s, job, c, in := childDeliveryFixture(t)
	_, err := s.PrepareEphemeralChildDelivery(ctx, job.ID, in, childAllow)
	mustNil(t, err)
	c.State = "running"
	c.Revision++
	mustNil(t, recordExecutorFailure(t, s, ctx, job.ID, "Explicit local executor rejection; no physical conclusion", childAllow))
	_, err = s.JoinSwarmChildren(ctx, job.Parent, childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	if _, err := s.CompleteSwarmChild(ctx, job.Parent, c.RequestID, c.Executor, c.Revision, "success", "Late report", childAllow); err == nil {
		t.Fatal("late actor report reversed failed join")
	}
	if _, err := s.db.Exec(`UPDATE swarm_child_tasks SET state='completed',outcome='success',report='late SQL',revision=revision+1,finished_at='now' WHERE id=?`, c.ID); err == nil {
		t.Fatal("SQL reversed server failure")
	}
	if err := s.guardChildLifecycle(ctx, c.Executor.ActorUID); err == nil {
		t.Fatal("logical failure released physical child hold")
	}
	a, err := s.GetManagedActor(ctx, c.Executor.ActorUID)
	mustNil(t, err)
	if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "auto-delete", "delete"); err == nil {
		t.Fatal("failure granted automatic cleanup")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("late report or cleanup refusal rewrote history")
	}
	sw, err := s.GetSwarm(ctx, a.SwarmID)
	mustNil(t, err)
	_, err = s.AcquireOwnerActorOperation(ctx, a, "owner-after-failure", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
}

func TestEphemeralExecutorFailureConcurrentImmutableAndReopen(t *testing.T) {
	ctx := context.Background()
	s, job, c := registeredFailureFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.RecordEphemeralChildExecutorFailure(ctx, job.ID, "Same local decision", childAllow)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		mustNil(t, err)
	}
	fact, err := s.GetEphemeralChildExecutorFailure(ctx, job.ID)
	mustNil(t, err)
	if fact.IntentID != job.ID || fact.ChildID != c.ID || fact.RequestID != job.RequestID || fact.Reason != "Same local decision" || fact.FailedAt == "" {
		t.Fatalf("wrong retained fact: %+v", fact)
	}
	before := databaseRows(t, s.db)
	for _, q := range []string{`UPDATE ephemeral_child_executor_failures SET reason='changed' WHERE intent_id=?`, `DELETE FROM ephemeral_child_executor_failures WHERE intent_id=?`, `INSERT OR REPLACE INTO ephemeral_child_executor_failures SELECT * FROM ephemeral_child_executor_failures WHERE intent_id=?`} {
		if _, err := s.db.Exec(q, job.ID); err == nil {
			t.Fatal("immutable fact changed")
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("immutable refusal changed rows")
	}
	var path string
	mustNil(t, s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	got, err := reopened.GetEphemeralChildExecutorFailure(ctx, job.ID)
	mustNil(t, err)
	if got != fact || !reflect.DeepEqual(before, databaseRows(t, reopened.db)) {
		t.Fatal("reopen lost immutable decision")
	}
}

func TestEphemeralExecutorFailureMixedRealResultSeal(t *testing.T) {
	ctx := context.Background()
	s, failed := allocationFixture(t, 2)
	success, err := s.EnqueueEphemeralChild(ctx, failed.Parent, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "successful-job", "Independent result", "pool", childAllow)
	mustNil(t, err)
	for n, job := range []EphemeralChildIntent{failed, success} {
		uid := fmt.Sprintf("mixed-%d", n)
		addEphemeralCapacity(t, s)
		_, err := s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation(uid, 303+n), childAllow, ephemeralAdmissionAllow)
		mustNil(t, err)
		setActor(t, s, uid, "phase='running',stage='done',operation='start'")
		attestForRegistration(t, s, job)
		_, err = s.RegisterEphemeralChild(ctx, job.ID, childAllow)
		mustNil(t, err)
	}
	mustNil(t, s.RecordEphemeralChildExecutorFailure(ctx, failed.ID, "Local validation rejected executor", childAllow))
	j, err := s.GetSwarmChildJoin(ctx, failed.Parent, childAllow)
	mustNil(t, err)
	var c SwarmChildTask
	for _, candidate := range j.Children {
		if candidate.RequestID == success.RequestID {
			c = candidate
		}
	}
	in := ActorDeliveryInput{SwarmID: c.Executor.SwarmID, SourceKind: "child", SourceID: c.ID, RequestID: c.RequestID, Commit: success.SourceCommit, ActorUID: c.Executor.ActorUID, Incarnation: c.Executor.Incarnation, AtespaceUID: c.Executor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"to":"mixed-1","body":"exact instruction","waitSec":0}`}
	_, err = s.PrepareEphemeralChildDelivery(ctx, success.ID, in, childAllow)
	mustNil(t, err)
	_, err = s.CompleteSwarmChild(ctx, success.Parent, c.RequestID, c.Executor, c.Revision+1, "success", "A real independent actor result", childAllow)
	mustNil(t, err)
	_, err = s.JoinSwarmChildren(ctx, success.Parent, childAllow)
	mustNil(t, err)
	mustNil(t, s.SealEphemeralChildResult(ctx, success.ID, childAllow))
	if err := s.SealEphemeralChildResult(ctx, failed.ID, childAllow); err == nil {
		t.Fatal("server failure received a result seal")
	}
}

func seedEphemeralChildExecutorFailureFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	swarmID := "fixture-executor-failure"
	mustNil(t, s.CreateSwarm(ctx, Swarm{ID: swarmID, ProjectID: "two", OwnerID: "u", Name: swarmID, AtespaceName: swarmID, AtespaceUID: ptr("native-" + swarmID), State: "active"}))
	addEphemeralCapacity(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("fixture-failure-parent", swarmID, 401))
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "fixture-failure-parent-start", "start")
	mustNil(t, err)
	a = lifecycleBind(t, s, a, "fixture-parent-start-task")
	a = lifecycleConfirm(t, s, a, "running")
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: swarmID, Role: "build", ActorUID: a.UID}))
	_, err = s.StartSwarm(ctx, SwarmStart{SwarmID: swarmID, UserID: "u", Definition: "build-first", Goal: "Retain a registered server failure, not an actor report.", Commit: startCommit}, allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, swarmID, "fixture-executor-failure-attempt", "swarm:"+swarmID)
	mustNil(t, err)
	mustNil(t, s.RecordActorStepAttestation(ctx, attempt, startOrigin, startCommit))
	op, err := s.CurrentActorOperation(ctx, a.UID)
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	parent := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "two", SwarmID: swarmID, AtespaceUID: a.AtespaceUID, ActorUID: a.UID, Incarnation: a.Incarnation}, Pass: attempt.Pass, Sequence: attempt.Sequence, RequestID: attempt.RequestID}
	job, err := s.EnqueueEphemeralChild(ctx, parent, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, "fixture-server-failed-executor", "Retain the original registered child unchanged.", "pool", childAllow)
	mustNil(t, err)
	addEphemeralCapacity(t, s)
	allocation, err := s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation("fixture-server-failed-worker", 402), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	_, err = s.ReserveEphemeralChildCreate(ctx, job.ID, childAllow)
	mustNil(t, err)
	a, err = s.GetManagedActor(ctx, allocation.ActorUID)
	mustNil(t, err)
	a = lifecycleBind(t, s, a, "fixture-failed-worker-create-task")
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.BeginEphemeralChildStart(ctx, job.ID, childAllow)
	mustNil(t, err)
	_, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, a.Incarnation, a.OperationID, "start", a.Revision, childAllow)
	mustNil(t, err)
	a, err = s.GetManagedActor(ctx, a.UID)
	mustNil(t, err)
	a = lifecycleBind(t, s, a, "fixture-failed-worker-start-task")
	lifecycleConfirm(t, s, a, "running")
	attestForRegistration(t, s, job)
	_, err = s.RegisterEphemeralChild(ctx, job.ID, childAllow)
	mustNil(t, err)
	mustNil(t, s.RecordEphemeralChildExecutorFailure(ctx, job.ID, "Fixture server explicitly rejected the registered executor.", childAllow))
}

func TestEphemeralExecutorFailureOfficial57Fixture(t *testing.T) {
	db := rawDatabase(t, loadSchemaFixture(t, 57))
	defer db.Close()
	var reason, state, report, delivery string
	mustNil(t, db.QueryRow(`SELECT f.reason,c.state,coalesce(c.report,''),c.delivery FROM ephemeral_child_executor_failures f JOIN swarm_child_tasks c ON c.id=f.child_id`).Scan(&reason, &state, &report, &delivery))
	if !strings.Contains(reason, "Fixture") || state != "pending" || report != "" || delivery != "" {
		t.Fatal("fixture lacks real server decision or invented report")
	}
}
