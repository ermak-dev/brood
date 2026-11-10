package store

// FEATURE: Доставка инструкции узла привязанному актору (#212), слой хранилища.
// SPEC: Шаг actor-узла роя с источником идёт pending → attesting →
// delivering → inflight. Резерв попытки (привязанный актор, его инкарнация,
// request_id и workspace-lease) пишется одной транзакцией до любого эффекта
// у гостя; повтор резерва ничего не пишет. Только запись подтверждения
// workspace с commit источника открывает шаг для доставки и исхода
// (ErrPipelineSourceUnverified снимается для этого шага). Неизвестный исход
// доставки — unconfirmed: граф стоит, резерв не освобождается, исход актора
// не принимается. Отказ, после которого инструкция заведомо не ушла, —
// refused с причиной. Исход принимается только от привязанного к роли
// актора той же инкарнации и только для идущего шага.
// IMPLEMENTATION: swarm_dispatch.go, CompletePipelineActorVisit, StopSwarm.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// dispatchStore — startStore со вторым актором b того же роя; оба готовы к
// управлению workspace. Рой «one» запущен на build-first: первый шаг build
// ждёт доставки, роль build привязана к a.
func dispatchStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := startStore(t)
	if _, err := s.ReserveManagedActor(ctx, managedRequest("b", "one", 202)); err != nil {
		t.Fatal(err)
	}
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	setActor(t, s, "b", "phase='running', stage='done', operation='start'")
	if _, err := startOne(s, "build-first"); err != nil {
		t.Fatal(err)
	}
	return s
}

func reserveOne(t *testing.T, s *Store, requestID string) ActorStepAttempt {
	t.Helper()
	a, err := s.ReserveActorStep(context.Background(), "one", requestID, "swarm:one")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return a
}

// attestOne — резерв и подтверждение commit источника: шаг уходит в delivering.
func attestOne(t *testing.T, s *Store, requestID string) ActorStepAttempt {
	t.Helper()
	a := reserveOne(t, s, requestID)
	mustNil(t, s.RecordActorStepAttestation(context.Background(), a, startOrigin, startCommit))
	return a
}

func stepOf(t *testing.T, s *Store, sequence int64) PipelineVisit {
	t.Helper()
	steps, err := s.SwarmSteps(context.Background(), "one")
	mustNil(t, err)
	for _, v := range steps {
		if v.Sequence == sequence {
			return v
		}
	}
	t.Fatalf("no step %d: %+v", sequence, steps)
	return PipelineVisit{}
}

func stepCount(t *testing.T, s *Store) int {
	t.Helper()
	steps, err := s.SwarmSteps(context.Background(), "one")
	mustNil(t, err)
	return len(steps)
}

// TestReserveActorStepRecordsAttempt — резерв пишет привязанного актора, его
// инкарнацию и request_id в шаг и берёт workspace-lease одной транзакцией;
// из одновременных резервов проходит один, повтор ничего не пишет.
func TestReserveActorStepRecordsAttempt(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ReserveActorStep(ctx, "one", "req-"+string(rune('a'+i)), "swarm:one")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	reserved := 0
	for err := range results {
		switch {
		case err == nil:
			reserved++
		case errors.Is(err, ErrNoActorStep):
		default:
			t.Fatalf("concurrent reserve: %v", err)
		}
	}
	if reserved != 1 || stepCount(t, s) != 1 {
		t.Fatalf("concurrent reserves: %d reserved, %d steps", reserved, stepCount(t, s))
	}
	v := stepOf(t, s, 1)
	if v.State != "attesting" || v.ActorUID != "a" || v.ActorIncarnation != 1 || !strings.HasPrefix(v.RequestID, "req-") || v.StartedAt == nil || v.AttestedCommit != "" {
		t.Fatalf("reserved step: %+v", v)
	}
	op, err := s.CurrentActorOperation(ctx, "a")
	if err != nil || op.OperationID != v.RequestID || op.Operation != "workspace_attest" || op.State != "inflight" {
		t.Fatalf("workspace lease: %+v %v", op, err)
	}
	before := dumpTables(t, s, append(executionTables, "coordination_claims")...)
	if _, err := s.ReserveActorStep(ctx, "one", "again", "swarm:one"); !errors.Is(err, ErrNoActorStep) {
		t.Fatalf("repeated reserve: %v", err)
	}
	if after := dumpTables(t, s, append(executionTables, "coordination_claims")...); after != before {
		t.Fatalf("repeated reserve wrote:\n%s\nwant\n%s", after, before)
	}
}

// TestReserveActorStepRefusals — роль не разрешается в живого привязанного
// актора или его workspace занят: шаг закрыт refused с причиной, актор в шаг
// не записан, lease не взят.
func TestReserveActorStepRefusals(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, reason string
		setup        func(*testing.T, *Store)
	}{
		{"unbound role", "not bound", func(t *testing.T, s *Store) {
			_, err := s.db.Exec(`DELETE FROM swarm_role_bindings WHERE swarm_id='one'`)
			mustNil(t, err)
		}},
		{"stale incarnation", "stale", func(t *testing.T, s *Store) { setActor(t, s, "a", "incarnation=2") }},
		{"stopped actor", "inactive", func(t *testing.T, s *Store) { setActor(t, s, "a", "phase='stopped'") }},
		{"workspace held", "workspace operation", func(t *testing.T, s *Store) {
			_, err := s.AcquireActorOperation(ctx, "a", 1, "held", "operator", "restart")
			mustNil(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dispatchStore(t)
			tc.setup(t, s)
			_, err := s.ReserveActorStep(ctx, "one", "req", "swarm:one")
			if !errors.Is(err, ErrActorStepRefused) {
				t.Fatalf("reserve: %v", err)
			}
			v := stepOf(t, s, 1)
			if v.State != "refused" || v.ActorUID != "" || v.RequestID != "" || !strings.Contains(v.Detail, tc.reason) || v.FinishedAt == nil {
				t.Fatalf("refused step: %+v", v)
			}
			if op, err := s.CurrentActorOperation(ctx, "a"); err == nil && op.OperationID == "req" {
				t.Fatalf("refused reserve kept a lease: %+v", op)
			}
			if _, err := s.ReserveActorStep(ctx, "one", "next", "swarm:one"); !errors.Is(err, ErrNoActorStep) {
				t.Fatalf("refused step reserved again: %v", err)
			}
		})
	}
}

// TestReserveActorStepWritesOnlyPending — записи резерва сами условны на
// pending: шаг в другом состоянии они не меняют, даже когда вызывающий не
// сверил состояние своей выборкой. Это не держится на _txlock=immediate:
// записи зовутся здесь прямо, мимо выборки ReserveActorStep.
func TestReserveActorStepWritesOnlyPending(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"attesting", "waiting", "unconfirmed", "refused", "completed", "stopped"} {
		t.Run(state, func(t *testing.T) {
			s := dispatchStore(t)
			_, err := s.db.Exec(`UPDATE swarm_steps SET state=?,actor_uid='b',actor_incarnation=1,request_id='earlier' WHERE swarm_id='one' AND sequence=1`, state)
			mustNil(t, err)
			actor, err := s.GetManagedActor(ctx, "a")
			mustNil(t, err)
			before := dumpTables(t, s, executionTables...)
			err = s.WithTx(ctx, func(ctx context.Context) error { return s.markStepAttesting(ctx, "one", 1, actor, "req") })
			if !errors.Is(err, ErrNoActorStep) {
				t.Fatalf("attesting write over %s: %v", state, err)
			}
			mustNil(t, s.WithTx(ctx, func(ctx context.Context) error { return s.refusePendingStep(ctx, "one", 1, "late refusal") }))
			if after := dumpTables(t, s, executionTables...); after != before {
				t.Fatalf("reservation writes changed a %s step:\n%s\nwant\n%s", state, after, before)
			}
		})
	}
}

// TestCheckActorStepAttestation — перед вызовом гостя попытка обязана быть
// attesting той же попытки, актора и инкарнации при идущем рое и роли,
// привязанной к этому актору; проверка ничего не пишет.
func TestCheckActorStepAttestation(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "req")
	before := dumpTables(t, s, append(executionTables, "coordination_claims")...)
	mustNil(t, s.CheckActorStepAttestation(ctx, a))
	other := a
	other.RequestID = "forged"
	if err := s.CheckActorStepAttestation(ctx, other); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("another attempt: %v", err)
	}
	if after := dumpTables(t, s, append(executionTables, "coordination_claims")...); after != before {
		t.Fatalf("check wrote:\n%s\nwant\n%s", after, before)
	}
	for name, drift := range map[string]func(*testing.T, *Store, ActorStepAttempt){
		"stopped": func(t *testing.T, s *Store, _ ActorStepAttempt) {
			_, err := s.StopSwarm(ctx, "one", "u", allow)
			mustNil(t, err)
		},
		"role rebound": func(t *testing.T, s *Store, _ ActorStepAttempt) {
			mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "b"}))
		},
		"attested": func(t *testing.T, s *Store, a ActorStepAttempt) {
			mustNil(t, s.RecordActorStepAttestation(ctx, a, startOrigin, startCommit))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := dispatchStore(t)
			a := reserveOne(t, s, "req")
			drift(t, s, a)
			if err := s.CheckActorStepAttestation(ctx, a); err == nil {
				t.Fatal("attestation allowed after the step changed")
			}
		})
	}
}

// TestWithdrawActorStep — попытка, которая не дошла до гостя (сервер
// выключается), возвращается в pending целиком: без актора, попытки, начала
// и причины, а её workspace-lease отпускается той же транзакцией. Следующий
// резерв берёт шаг заново. Шаг другой попытки или уже не attesting не
// меняется, lease остаётся.
func TestWithdrawActorStep(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "req")
	mustNil(t, s.WithdrawActorStep(ctx, a))
	v := stepOf(t, s, 1)
	if v.State != "pending" || v.ActorUID != "" || v.ActorIncarnation != 0 || v.RequestID != "" || v.StartedAt != nil || v.Detail != "" || v.FinishedAt != nil {
		t.Fatalf("withdrawn step: %+v", v)
	}
	if _, err := s.CurrentActorOperation(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lease after withdrawal: %v", err)
	}
	if pending, err := s.PendingActorStepSwarms(ctx); err != nil || len(pending) != 1 || pending[0] != "one" {
		t.Fatalf("withdrawn step not pending for dispatch: %v %v", pending, err)
	}
	if err := s.WithdrawActorStep(ctx, a); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("repeated withdrawal: %v", err)
	}
	if again := reserveOne(t, s, "next"); again.RequestID != "next" || stepCount(t, s) != 1 {
		t.Fatalf("withdrawn step reserved again: %+v", again)
	}

	for name, drift := range map[string]func(*testing.T, *Store, ActorStepAttempt) ActorStepAttempt{
		"stopped": func(t *testing.T, s *Store, a ActorStepAttempt) ActorStepAttempt {
			_, err := s.StopSwarm(ctx, "one", "u", allow)
			mustNil(t, err)
			return a
		},
		"attested": func(t *testing.T, s *Store, a ActorStepAttempt) ActorStepAttempt {
			mustNil(t, s.RecordActorStepAttestation(ctx, a, startOrigin, startCommit))
			return a
		},
		"another attempt": func(_ *testing.T, _ *Store, a ActorStepAttempt) ActorStepAttempt {
			a.RequestID = "forged"
			return a
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := dispatchStore(t)
			a := reserveOne(t, s, "req")
			withdrawn := drift(t, s, a)
			before := dumpTables(t, s, append(executionTables, "coordination_claims")...)
			if err := s.WithdrawActorStep(ctx, withdrawn); !errors.Is(err, ErrPipelineVisitConflict) {
				t.Fatalf("withdrawal: %v", err)
			}
			if after := dumpTables(t, s, append(executionTables, "coordination_claims")...); after != before {
				t.Fatalf("refused withdrawal wrote:\n%s\nwant\n%s", after, before)
			}
		})
	}
}

// TestReserveActorStepIgnoresUnsourcedSwarm — граф без снимка источника
// диспетчер не трогает: такой рой создаётся только мимо запуска (тесты и
// базы до #198), его шаги остаются прежнему пути.
func TestReserveActorStepIgnoresUnsourcedSwarm(t *testing.T) {
	ctx := context.Background()
	s := swarmTestStore(t, "r")
	mustNil(t, attachTestGraph(ctx, s, "r", actorStartGraph))
	roleActor(t, s, "r", "a", "build")
	_, err := s.db.Exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('r',1,'actor','pending')`)
	mustNil(t, err)
	if _, err := s.ReserveActorStep(ctx, "r", "req", "swarm:r"); !errors.Is(err, ErrNoActorStep) {
		t.Fatalf("unsourced reserve: %v", err)
	}
	steps, err := s.SwarmSteps(ctx, "r")
	if err != nil || len(steps) != 1 || steps[0].State != "pending" {
		t.Fatalf("unsourced step changed: %+v %v", steps, err)
	}
}

// TestActorStepAttestationGate — до записи подтверждения шаг роя с
// источником не принимает исход (ErrPipelineSourceUnverified); запись
// принимает только commit и origin источника и только для той же попытки
// привязанного актора идущего роя; после неё шаг в delivering с commit.
func TestActorStepAttestationGate(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a := reserveOne(t, s, "req")
	if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "success"); !errors.Is(err, ErrPipelineSourceUnverified) {
		t.Fatalf("completion before attestation: %v", err)
	}
	for name, record := range map[string]func() error{
		"other commit": func() error { return s.RecordActorStepAttestation(ctx, a, startOrigin, strings.Repeat("b", 40)) },
		"other origin": func() error {
			return s.RecordActorStepAttestation(ctx, a, "https://git.example.org/team/other.git", startCommit)
		},
		"other attempt": func() error {
			b := a
			b.RequestID = "forged"
			return s.RecordActorStepAttestation(ctx, b, startOrigin, startCommit)
		},
		"other actor": func() error {
			b := a
			b.Actor.UID = "b"
			return s.RecordActorStepAttestation(ctx, b, startOrigin, startCommit)
		},
		"other incarnation": func() error {
			b := a
			b.Actor.Incarnation = 2
			return s.RecordActorStepAttestation(ctx, b, startOrigin, startCommit)
		},
	} {
		if err := record(); err == nil {
			t.Fatalf("%s: attestation recorded", name)
		}
		if v := stepOf(t, s, 1); v.State != "attesting" || v.AttestedCommit != "" {
			t.Fatalf("%s: step changed: %+v", name, v)
		}
	}
	mustNil(t, s.RecordActorStepAttestation(ctx, a, startOrigin, startCommit))
	if v := stepOf(t, s, 1); v.State != "delivering" || v.AttestedCommit != startCommit {
		t.Fatalf("attested step: %+v", v)
	}
	if err := s.RecordActorStepAttestation(ctx, a, startOrigin, startCommit); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("repeated attestation: %v", err)
	}
	mustNil(t, s.CheckActorStepDelivery(ctx, a))
}

// TestActorStepAttestationRechecksRole — привязку роли запись подтверждения
// проверяет заново: роль, переназначенная или устаревшая во время
// подтверждения, шаг до доставки не допускает.
func TestActorStepAttestationRechecksRole(t *testing.T) {
	ctx := context.Background()
	for name, drift := range map[string]func(*testing.T, *Store){
		"rebound": func(t *testing.T, s *Store) {
			mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "b"}))
		},
		"another incarnation": func(t *testing.T, s *Store) { setActor(t, s, "a", "incarnation=2") },
	} {
		t.Run(name, func(t *testing.T) {
			s := dispatchStore(t)
			a := reserveOne(t, s, "req")
			drift(t, s)
			if err := s.RecordActorStepAttestation(ctx, a, startOrigin, startCommit); err == nil {
				t.Fatal("attestation recorded for a role that moved")
			}
			mustNil(t, s.RefuseActorStep(ctx, a, "the role binding changed during workspace attestation"))
			if v := stepOf(t, s, 1); v.State != "refused" || v.AttestedCommit != "" || !strings.Contains(v.Detail, "role binding changed") {
				t.Fatalf("step after drift: %+v", v)
			}
		})
	}
}

// TestActorStepDeliveryOutcomes — подтверждённая доставка переводит шаг в
// inflight, и только тогда (или в окне delivering) актор отчитывается;
// неизвестный исход — unconfirmed: граф стоит, резерв остаётся, исход
// актора не принимается, повтор попытки ничего не пишет.
func TestActorStepDeliveryOutcomes(t *testing.T) {
	ctx := context.Background()
	t.Run("delivered", func(t *testing.T) {
		s := dispatchStore(t)
		a := attestOne(t, s, "req")
		mustNil(t, s.FinishActorStepDelivery(ctx, a))
		if v := stepOf(t, s, 1); v.State != "inflight" || v.Detail != "" {
			t.Fatalf("delivered step: %+v", v)
		}
		if err := s.FinishActorStepDelivery(ctx, a); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("repeated delivery record: %v", err)
		}
		if _, err := s.ReserveActorStep(ctx, "one", "again", "swarm:one"); !errors.Is(err, ErrNoActorStep) {
			t.Fatalf("delivered step reserved again: %v", err)
		}
		mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "success"))
		if w, err := s.GetSwarm(ctx, "one"); err != nil || w.Execution != "succeeded" || stepCount(t, s) != 2 {
			t.Fatalf("completion did not advance: %+v %v", w, err)
		}
	})
	t.Run("completion while delivering", func(t *testing.T) {
		s := dispatchStore(t)
		a := attestOne(t, s, "req")
		// Актор получил инструкцию и ответил раньше, чем сервер записал итог.
		mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "success"))
		if err := s.FinishActorStepDelivery(ctx, a); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("late delivery record: %v", err)
		}
		if v := stepOf(t, s, 1); v.State != "completed" {
			t.Fatalf("late delivery record changed the step: %+v", v)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		s := dispatchStore(t)
		a := attestOne(t, s, "req")
		mustNil(t, s.UnconfirmActorStep(ctx, a, "instruction delivery outcome is unknown"))
		before := dumpTables(t, s, executionTables...)
		v := stepOf(t, s, 1)
		if v.State != "unconfirmed" || v.ActorUID != "a" || v.ActorIncarnation != 1 || v.RequestID != "req" || v.AttestedCommit != startCommit || !strings.Contains(v.Detail, "unknown") {
			t.Fatalf("unconfirmed step: %+v", v)
		}
		if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "success"); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("completion of an unconfirmed step: %v", err)
		}
		for name, replay := range map[string]func() error{
			"finish":  func() error { return s.FinishActorStepDelivery(ctx, a) },
			"refuse":  func() error { return s.RefuseActorStep(ctx, a, "late refusal") },
			"reserve": func() error { _, err := s.ReserveActorStep(ctx, "one", "again", "swarm:one"); return err },
			"recover": func() error { return s.RecoverActorSteps(ctx) },
		} {
			_ = replay()
			if after := dumpTables(t, s, executionTables...); after != before {
				t.Fatalf("%s after an unknown delivery wrote:\n%s\nwant\n%s", name, after, before)
			}
		}
		if w, err := s.GetSwarm(ctx, "one"); err != nil || w.Execution != "running" {
			t.Fatalf("unknown delivery moved the swarm: %+v %v", w, err)
		}
	})
}

// TestActorStepCompletionIdentity — исход принимается только от актора,
// привязанного к роли узла, в той же инкарнации и только для идущего шага.
func TestActorStepCompletionIdentity(t *testing.T) {
	ctx := context.Background()
	t.Run("role moved to another actor", func(t *testing.T) {
		s := dispatchStore(t)
		a := attestOne(t, s, "req")
		mustNil(t, s.FinishActorStepDelivery(ctx, a))
		mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "b"}))
		if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "success"); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("completion from an actor no longer bound: %v", err)
		}
		if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "b", 1, "success"); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("completion from the new actor of the role: %v", err)
		}
		if v := stepOf(t, s, 1); v.State != "inflight" || stepCount(t, s) != 1 {
			t.Fatalf("refused completion changed the graph: %+v", v)
		}
	})
	t.Run("another incarnation", func(t *testing.T) {
		s := dispatchStore(t)
		a := attestOne(t, s, "req")
		mustNil(t, s.FinishActorStepDelivery(ctx, a))
		setActor(t, s, "a", "incarnation=2")
		rebindAt(t, s, "one", "a", "build", 2)
		for _, incarnation := range []int64{1, 2} {
			if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", incarnation, "success"); !errors.Is(err, ErrPipelineVisitConflict) {
				t.Fatalf("completion from incarnation %d: %v", incarnation, err)
			}
		}
	})
	t.Run("finished step", func(t *testing.T) {
		s := dispatchStore(t)
		a := attestOne(t, s, "req")
		mustNil(t, s.FinishActorStepDelivery(ctx, a))
		mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "fail"))
		before := dumpTables(t, s, executionTables...)
		if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "success"); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("second outcome of a finished step: %v", err)
		}
		// Точный повтор подтверждает уже записанный исход и ничего не пишет.
		mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "fail"))
		if after := dumpTables(t, s, executionTables...); after != before {
			t.Fatalf("outcome for a finished step wrote:\n%s\nwant\n%s", after, before)
		}
	})
}

// TestStopSwarmDuringAttestationAndDelivery — остановка закрывает шаг на
// подтверждении (stopped: инструкция ещё не ушла и уже не уйдёт), шаг на
// доставке становится unconfirmed; поздние записи подтверждения и доставки
// ничего не меняют, исход не принимается.
func TestStopSwarmDuringAttestationAndDelivery(t *testing.T) {
	ctx := context.Background()
	t.Run("attesting", func(t *testing.T) {
		s := dispatchStore(t)
		a := reserveOne(t, s, "req")
		_, err := s.StopSwarm(ctx, "one", "u", allow)
		mustNil(t, err)
		if v := stepOf(t, s, 1); v.State != "stopped" {
			t.Fatalf("stopped attestation: %+v", v)
		}
		if err := s.RecordActorStepAttestation(ctx, a, startOrigin, startCommit); err == nil {
			t.Fatal("attestation recorded after stop")
		}
		if err := s.CheckActorStepDelivery(ctx, a); err == nil {
			t.Fatal("delivery allowed after stop")
		}
		if v := stepOf(t, s, 1); v.State != "stopped" || v.AttestedCommit != "" {
			t.Fatalf("late attestation changed the step: %+v", v)
		}
	})
	t.Run("delivering", func(t *testing.T) {
		s := dispatchStore(t)
		a := attestOne(t, s, "req")
		_, err := s.StopSwarm(ctx, "one", "u", allow)
		mustNil(t, err)
		if v := stepOf(t, s, 1); v.State != "unconfirmed" || !strings.Contains(v.Detail, "stopped") {
			t.Fatalf("stopped delivery: %+v", v)
		}
		if err := s.CheckActorStepDelivery(ctx, a); err == nil {
			t.Fatal("delivery allowed after stop")
		}
		if err := s.FinishActorStepDelivery(ctx, a); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("delivery recorded after stop: %v", err)
		}
		if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "success"); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("completion after stop: %v", err)
		}
	})
}

// TestRecoverActorSteps — при старте сервера попытка с неизвестным исходом
// (attesting, delivering) становится unconfirmed, inflight и pending не
// меняются, pending-шаги находятся для обработки.
func TestRecoverActorSteps(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	reserveOne(t, s, "req")
	pending, err := s.PendingActorStepSwarms(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending with a reserved step: %v %v", pending, err)
	}
	mustNil(t, s.RecoverActorSteps(ctx))
	if v := stepOf(t, s, 1); v.State != "unconfirmed" || !strings.Contains(v.Detail, "restarted") {
		t.Fatalf("recovered attestation: %+v", v)
	}

	s = dispatchStore(t)
	pending, err = s.PendingActorStepSwarms(ctx)
	if err != nil || len(pending) != 1 || pending[0] != "one" {
		t.Fatalf("pending swarms: %v %v", pending, err)
	}
	mustNil(t, s.RecoverActorSteps(ctx))
	if v := stepOf(t, s, 1); v.State != "pending" {
		t.Fatalf("recovery changed a pending step: %+v", v)
	}
	a := attestOne(t, s, "req")
	mustNil(t, s.RecoverActorSteps(ctx))
	if v := stepOf(t, s, 1); v.State != "unconfirmed" || !strings.Contains(v.Detail, "restarted") {
		t.Fatalf("recovered delivery: %+v", v)
	}
	if err := s.FinishActorStepDelivery(ctx, a); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("delivery recorded after recovery: %v", err)
	}

	s = dispatchStore(t)
	a = attestOne(t, s, "req")
	mustNil(t, s.FinishActorStepDelivery(ctx, a))
	mustNil(t, s.RecoverActorSteps(ctx))
	if v := stepOf(t, s, 1); v.State != "inflight" {
		t.Fatalf("recovery changed a delivered step: %+v", v)
	}
}

// TestSwarmStepSchemaHoldsAttestation — схема держит словарь состояний шага,
// предел причины и правило: шаг роя с источником не бывает delivering или
// inflight без подтверждённого commit этого источника.
func TestSwarmStepSchemaHoldsAttestation(t *testing.T) {
	for _, tc := range []struct{ name, statement string }{
		{"unknown state", `UPDATE swarm_steps SET state='invented' WHERE swarm_id='one'`},
		{"delivering unattested", `UPDATE swarm_steps SET state='delivering' WHERE swarm_id='one'`},
		{"inflight other commit", `UPDATE swarm_steps SET state='inflight',attested_commit='` + strings.Repeat("b", 40) + `' WHERE swarm_id='one'`},
		{"inserted inflight", `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('one',9,'build','inflight')`},
		{"malformed commit", `UPDATE swarm_steps SET attested_commit='HEAD' WHERE swarm_id='one'`},
		{"oversized detail", `UPDATE swarm_steps SET detail='` + strings.Repeat("x", 1025) + `' WHERE swarm_id='one'`},
		{"attestation dropped", `UPDATE swarm_steps SET attested_commit=NULL WHERE swarm_id='one'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := dispatchStore(t)
			if tc.name == "attestation dropped" {
				attestOne(t, s, "req")
			} else {
				reserveOne(t, s, "req")
			}
			if _, err := s.db.Exec(tc.statement); err == nil {
				t.Fatalf("%s accepted", tc.name)
			}
		})
	}
}
