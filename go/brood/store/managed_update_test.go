package store

// FEATURE: update restarts a running managed actor in place (#133).
// SPEC: The update intent is the start operation of the same incarnation with
// a stop stage first: CTID, owner nonce, generation, resources and template
// stay, desired stays running. A confirmed stop moves the slot to the start
// stage; a confirmed absence settles it as an error, never a start. Only a
// running, reconciled actor of an enabled pool is updated. The intent is
// refused, with nothing written, while the actor holds or is the target of a
// dispatch lease, holds a landing token, has a pipeline step in progress (the
// latest step of its running swarm is pending for the role bound to it, or
// attesting, delivering, inflight or unconfirmed on this incarnation; the
// refusal names the step), or while its swarm runs a pipeline pass at all
// (running or waiting): a step that became pending for the actor's role during
// the restart would be closed as refused. That refusal names the swarm. A
// finished pass and a stopped swarm do not block.
// IMPLEMENTATION: BeginManagedActorOperation "update", ConfirmManagedActorStage,
// guardActorPipeline.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// runningActor — актор a пула pool, запущенный и согласованный.
func runningActor(t *testing.T) (*Store, ManagedActor) {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	if _, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101)); err != nil {
		t.Fatal(err)
	}
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	a, err := s.GetManagedActor(ctx, "a")
	mustNil(t, err)
	return s, a
}

func TestManagedUpdateRestartsInPlace(t *testing.T) {
	s, before := runningActor(t)
	a := lifecycleBegin(t, s, before, "update")
	if a.Operation != "start" || a.Desired != "running" || a.Stage != "stop" || a.Phase != "stopping" || a.OperationID != "intent-update" || a.UPID != "" {
		t.Fatalf("update intent: %+v", a)
	}
	if a.Incarnation != before.Incarnation || a.CTID != before.CTID || a.OwnerNonce != before.OwnerNonce || a.Resources != before.Resources || a.Template != before.Template || a.PoolUID != before.PoolUID {
		t.Fatalf("update changed the allocation: %+v -> %+v", before, a)
	}
	a = lifecycleBind(t, s, a, "UPID:stop")
	if _, err := s.ConfirmManagedActorStage(context.Background(), a.UID, a.Revision, a.OperationID, a.Stage, "running"); err == nil {
		t.Fatal("a running CT confirmed the stop stage")
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	// Phase stopped у стадии start без задачи: CT остановлена, Start ещё не
	// запрошен, и reconcile может его отправить (#133).
	if a.Operation != "start" || a.Stage != "start" || a.Phase != "stopped" || a.UPID != "" || a.Error != "" || a.OperationID != "intent-update" || a.Desired != "running" {
		t.Fatalf("stop stage did not lead to start: %+v", a)
	}
	a = lifecycleBind(t, s, a, "UPID:start")
	a = lifecycleConfirm(t, s, a, "running")
	if a.Stage != "done" || a.Phase != "running" || a.Incarnation != before.Incarnation || a.OwnerNonce != before.OwnerNonce {
		t.Fatalf("restart did not settle in place: %+v", a)
	}
}

func TestManagedUpdateAbsentContainerIsNotStarted(t *testing.T) {
	s, before := runningActor(t)
	a := lifecycleBind(t, s, lifecycleBegin(t, s, before, "update"), "UPID:stop")
	a = lifecycleConfirm(t, s, a, "absent")
	if a.Stage != "done" || a.Phase != "error" || a.Operation != "start" {
		t.Fatalf("absent CT after the stop stage: %+v", a)
	}
}

func TestManagedUpdateRequiresRunningActor(t *testing.T) {
	for name, change := range map[string]string{
		"stopped":      "phase='stopped', desired='stopped', operation='stop'",
		"error":        "phase='error'",
		"provisioning": "phase='provisioning'",
		"unreconciled": "stage='start'",
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := runningActor(t)
			setActor(t, s, "a", change)
			a, err := s.GetManagedActor(context.Background(), "a")
			mustNil(t, err)
			if _, err := s.BeginManagedActorOperation(context.Background(), "a", a.Revision, "update", "update"); err == nil {
				t.Fatal("update of an actor that is not running accepted")
			}
			after, err := s.GetManagedActor(context.Background(), "a")
			if err != nil || !reflect.DeepEqual(after, a) {
				t.Fatalf("refused update changed the actor: %+v %v", after, err)
			}
		})
	}
	t.Run("draining pool", func(t *testing.T) {
		s, a := runningActor(t)
		if _, err := s.db.Exec(`UPDATE managed_pools SET state='draining'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BeginManagedActorOperation(context.Background(), "a", a.Revision, "update", "update"); !errors.Is(err, ErrPoolUnavailable) {
			t.Fatalf("update in a draining pool: %v", err)
		}
	})
}

func TestManagedUpdateRefusesLandingToken(t *testing.T) {
	s := newTestStore(t)
	c := coordinationFixture(t, s)
	c.RequestID, c.Target = "land-133", CoordinationActor{}
	ctx := context.Background()
	if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase='running',operation='start',stage='done'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeLandingToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	actor, err := s.GetManagedActor(ctx, "source")
	mustNil(t, err)
	want := `landing token "land-133" is held by "source"`
	if _, err := s.BeginManagedActorOperation(ctx, "source", actor.Revision, NewID(), "update"); !errors.Is(err, ErrCoordinationConflict) || !strings.Contains(err.Error(), want) {
		t.Fatalf("update of the landing holder: %v, want a refusal saying %s", err, want)
	}
}

// updateOf пробует намерение update актора uid.
func updateOf(t *testing.T, s *Store, uid string) error {
	t.Helper()
	a, err := s.GetManagedActor(context.Background(), uid)
	mustNil(t, err)
	_, err = s.BeginManagedActorOperation(context.Background(), uid, a.Revision, NewID(), "update")
	if err != nil {
		after, getErr := s.GetManagedActor(context.Background(), uid)
		if getErr != nil || !reflect.DeepEqual(after, a) {
			t.Fatalf("refused update changed the actor: %+v %v", after, getErr)
		}
	}
	return err
}

func TestManagedUpdateRefusesStepInProgress(t *testing.T) {
	ctx := context.Background()
	// Резерв попытки держит workspace-lease подтверждения; сервер снимает
	// его после ответа гостя. Без lease отказ даёт только шаг.
	attested := func(t *testing.T, s *Store) ActorStepAttempt {
		a := attestOne(t, s, "req")
		mustNil(t, s.FinishActorOperation(ctx, a.Lease, true))
		return a
	}
	for state, prepare := range map[string]func(t *testing.T, s *Store){
		"pending":    func(*testing.T, *Store) {},
		"attesting":  func(t *testing.T, s *Store) { reserveOne(t, s, "req") },
		"delivering": func(t *testing.T, s *Store) { attested(t, s) },
		"inflight": func(t *testing.T, s *Store) {
			mustNil(t, s.FinishActorStepDelivery(ctx, attested(t, s)))
		},
		"unconfirmed": func(t *testing.T, s *Store) {
			mustNil(t, s.UnconfirmActorStep(ctx, attested(t, s), "delivery outcome unknown"))
		},
	} {
		t.Run(state, func(t *testing.T) {
			s := dispatchStore(t)
			prepare(t, s)
			if v := stepOf(t, s, 1); v.State != state {
				t.Fatalf("step fixture is %s, want %s", v.State, state)
			}
			want := `pipeline step 1 (node "build") is ` + state + ` for actor "a"; update restarts the actor and would interrupt it; wait for the step to finish or stop the swarm`
			if err := updateOf(t, s, "a"); !errors.Is(err, ErrCoordinationConflict) || !strings.Contains(err.Error(), want) {
				t.Fatalf("update with a %s step: %v\nwant a refusal saying %s", state, err, want)
			}
			// Шаг принадлежит роли a; актору b того же роя отказывает
			// идущий проход, и отказ называет рой, а не шаг.
			if err := updateOf(t, s, "b"); !errors.Is(err, ErrCoordinationConflict) || !strings.Contains(err.Error(), swarmPassRefusal("running", "b")) || strings.Contains(err.Error(), "pipeline step") {
				t.Fatalf("update of the actor without the step: %v", err)
			}
		})
	}
	t.Run("completed", func(t *testing.T) {
		s := dispatchStore(t)
		mustNil(t, s.FinishActorStepDelivery(ctx, attested(t, s)))
		mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "success", ""))
		if err := updateOf(t, s, "a"); err != nil {
			t.Fatalf("update after the step completed: %v", err)
		}
	})
	t.Run("stopped swarm", func(t *testing.T) {
		// Inflight-шаг остановленного роя уже не завершится: он не держит
		// актора вечно.
		s := dispatchStore(t)
		mustNil(t, s.FinishActorStepDelivery(ctx, attested(t, s)))
		if _, err := s.StopSwarm(ctx, "one", "u", allow); err != nil {
			t.Fatal(err)
		}
		if err := updateOf(t, s, "a"); err != nil {
			t.Fatalf("update after the swarm stopped: %v", err)
		}
	})
	t.Run("previous incarnation", func(t *testing.T) {
		// Шаг прежней инкарнации нынешний актор не продолжит и не завершит:
		// отказ даёт идущий проход, а не этот шаг.
		s := dispatchStore(t)
		mustNil(t, s.FinishActorStepDelivery(ctx, attested(t, s)))
		setActor(t, s, "a", "incarnation=2")
		if err := updateOf(t, s, "a"); !errors.Is(err, ErrCoordinationConflict) || !strings.Contains(err.Error(), swarmPassRefusal("running", "a")) || strings.Contains(err.Error(), "pipeline step") {
			t.Fatalf("update with a step of the previous incarnation: %v", err)
		}
	})
}

// swarmPassRefusal — отказ update актора actor, пока проход роя «one» в
// состоянии execution.
func swarmPassRefusal(execution, actor string) string {
	return `the pipeline pass of swarm "one" is ` + execution + `; update restarts actor "` + actor + `", and a step that becomes pending for its role during the restart would be refused; stop the swarm or wait for the pass to end`
}

// TestManagedUpdateRefusesRunningPass: пока проход роя идёт или ждёт
// оператора, update любого актора роя — отказ, хотя шага у актора нет.
// Проход human → build: оператор отвечает во время перезапуска build, и шаг
// build закрылся бы отказом «role has no live bound actor». Остановленный
// рой актора не держит.
func TestManagedUpdateRefusesRunningPass(t *testing.T) {
	ctx := context.Background()
	s := startStore(t)
	if _, err := s.ReserveManagedActor(ctx, managedRequest("b", "one", 202)); err != nil {
		t.Fatal(err)
	}
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	setActor(t, s, "b", "phase='running', stage='done', operation='start'")
	w, err := startOne(s, "release")
	mustNil(t, err)
	if w.Execution != "waiting" {
		t.Fatalf("release pass is %s, want waiting on the operator", w.Execution)
	}
	tables := []string{"managed_actors", "swarm_steps", "coordination_claims"}
	for _, uid := range []string{"a", "b"} {
		a, err := s.GetManagedActor(ctx, uid)
		mustNil(t, err)
		before := dumpTables(t, s, tables...)
		if err := s.CheckActorUpdate(ctx, a); !errors.Is(err, ErrCoordinationConflict) || !strings.Contains(err.Error(), swarmPassRefusal("waiting", uid)) {
			t.Fatalf("check of %s during a waiting pass: %v", uid, err)
		}
		if after := dumpTables(t, s, tables...); after != before {
			t.Fatalf("check of %s wrote:\n%s\n%s", uid, before, after)
		}
		if err := updateOf(t, s, uid); !errors.Is(err, ErrCoordinationConflict) || !strings.Contains(err.Error(), swarmPassRefusal("waiting", uid)) {
			t.Fatalf("update of %s during a waiting pass: %v", uid, err)
		}
	}
	stopOne(t, s)
	if err := updateOf(t, s, "a"); err != nil {
		t.Fatalf("update after the swarm stopped: %v", err)
	}
}

// TestCheckActorUpdateWritesNothing: проверка до вызова гостя называет ту же
// причину, что и намерение, и ничего не пишет; свободный актор проходит.
func TestCheckActorUpdateWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	tables := []string{"managed_actors", "swarm_steps", "coordination_claims"}
	check := func(uid, want string) {
		t.Helper()
		a, err := s.GetManagedActor(ctx, uid)
		mustNil(t, err)
		before := dumpTables(t, s, tables...)
		err = s.CheckActorUpdate(ctx, a)
		if want == "" && err != nil || want != "" && (!errors.Is(err, ErrCoordinationConflict) || !strings.Contains(err.Error(), want)) {
			t.Fatalf("check of %s: %v, want %q", uid, err, want)
		}
		if after := dumpTables(t, s, tables...); after != before {
			t.Fatalf("check of %s wrote:\n%s\n%s", uid, before, after)
		}
	}
	check("a", `pipeline step 1 (node "build") is pending for actor "a"`)
	check("b", swarmPassRefusal("running", "b"))
	// Остановленный рой актора не держит: свободный актор проходит.
	stopOne(t, s)
	check("b", "")
	c := CoordinationClaim{ProjectID: "one", SwarmID: "one", RequestID: "task-133", Source: CoordinationActor{UID: "a", Generation: 1}, Target: CoordinationActor{UID: "b", Generation: 1}, HumanID: "u"}
	if _, err := s.ClaimDispatchLease(ctx, c); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetManagedActor(ctx, "b")
	mustNil(t, err)
	if err := s.CheckActorUpdate(ctx, b); !errors.Is(err, ErrCoordinationConflict) || !strings.Contains(err.Error(), `dispatch lease "task-133" on "b" is held by "a"`) {
		t.Fatalf("check of a leased actor: %v", err)
	}
}
