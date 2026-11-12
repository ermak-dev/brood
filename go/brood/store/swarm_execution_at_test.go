package store

// FEATURE: Строка роя несёт момент последнего исполнения (#268).
// SPEC: момент последнего исполнения роя: начало текущего прохода, момент
// его остановки или окончание последнего прохода (succeeded или failed).
// Его пишет тот же UPDATE, что меняет исполнение на границе прохода: запуск,
// остановка и переход, кончающий проход. Переходы running ↔ waiting внутри
// прохода и записи роя вне исполнения (владелец, инфраструктура) его не
// трогают. У роя без прохода момента нет. Сводка списка роёв отдаёт его той
// же выборкой.
// IMPLEMENTATION: swarms.execution_at (схема 28): beginPass, StopSwarm,
// progressPipeline; SwarmSummary.ExecutionAt.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"strings"
	"testing"
	"time"
)

// startHandoff: актор, затем оператор; проход переходит из running в waiting.
const startHandoff = `apiVersion: brood.dev/v1alpha1
name: handoff
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: work}
  ask:
    type: human
    question: Merge?
    options:
      - {label: Merge, outcome: success}
  done: {type: exit}
edges:
  - {from: build, to: ask}
  - {from: ask, to: done}
goalGates: [done]
`

// startStrict: у отказа актора нет ребра, проход кончается терминальным
// отказом на шаге актора, без шага выхода.
const startStrict = `apiVersion: brood.dev/v1alpha1
name: strict
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: build, to: done, condition: "outcome=success"}
goalGates: [done]
`

// executionStore: startStore, в каталоге которого есть и handoff, и strict,
// а актор a готов принять инструкцию шага.
func executionStore(t *testing.T) *Store {
	t.Helper()
	s := startStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	publish(t, s, "one", map[string]string{"release": startRelease, "approve": startApprove, "build-first": startBuildFirst, "handoff": startHandoff, "strict": startStrict})
	return s
}

// executionAtOf: момент последнего исполнения роя из сводки списка роёв.
func executionAtOf(t *testing.T, s *Store, swarmID string) string {
	t.Helper()
	summaries, err := s.SwarmSummaries(context.Background(), []string{swarmID})
	mustNil(t, err)
	summary, ok := summaries[swarmID]
	if !ok {
		t.Fatalf("no summary of swarm %s", swarmID)
	}
	return summary.ExecutionAt
}

// requireMomentWithin: момент в RFC 3339 UTC между засечками до и после
// операции, которая его записала.
func requireMomentWithin(t *testing.T, what, at string, before, after time.Time) {
	t.Helper()
	moment, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || !strings.HasSuffix(at, "Z") {
		t.Fatalf("%s: executionAt %q is not RFC 3339 UTC: %v", what, at, err)
	}
	if moment.Before(before) || moment.After(after) {
		t.Fatalf("%s: executionAt %s is outside the operation [%s, %s]", what, at, before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
}

// completeBuild доставляет инструкцию шага актору a и записывает его исход.
func completeBuild(t *testing.T, s *Store, sequence int64, requestID, outcome string) {
	t.Helper()
	a := attestOne(t, s, requestID)
	mustNil(t, s.FinishActorStepDelivery(context.Background(), a))
	mustNil(t, s.CompletePipelineActorVisit(context.Background(), "one", "one", sequence, requestID, "a", 1, outcome, ""))
}

func requireExecution(t *testing.T, s *Store, want string) {
	t.Helper()
	w, err := s.GetSwarm(context.Background(), "one")
	mustNil(t, err)
	if w.Execution != want {
		t.Fatalf("execution = %q, want %q", w.Execution, want)
	}
}

func TestExecutionAtMarksTheStartOfAPass(t *testing.T) {
	s := executionStore(t)
	if at := executionAtOf(t, s, "one"); at != "" {
		t.Fatalf("a swarm without a pass has executionAt %q", at)
	}
	for _, definition := range []string{"release", "build-first"} {
		t.Run(definition, func(t *testing.T) {
			s := executionStore(t)
			before := time.Now().UTC()
			mustStart(t, s, definition)
			requireMomentWithin(t, "start of "+definition, executionAtOf(t, s, "one"), before, time.Now().UTC())
		})
	}
}

// TestExecutionAtKeepsTheStartInsideAPass: внутри прохода момент остаётся
// его началом, как бы граф ни переходил между running и waiting, и записи роя
// вне исполнения, двигающие updated_at, его не трогают.
func TestExecutionAtKeepsTheStartInsideAPass(t *testing.T) {
	ctx := context.Background()
	t.Run("waiting to running", func(t *testing.T) {
		s := executionStore(t)
		mustStart(t, s, "release")
		started := executionAtOf(t, s, "one")
		mustNil(t, answer(s, openNodeRequestOf(t, s, 1), "u", "success", ""))
		requireExecution(t, s, "running")
		if at := executionAtOf(t, s, "one"); at != started {
			t.Fatalf("waiting -> running moved executionAt from %s to %s", started, at)
		}
	})
	t.Run("running to waiting", func(t *testing.T) {
		s := executionStore(t)
		mustStart(t, s, "handoff")
		started := executionAtOf(t, s, "one")
		completeBuild(t, s, 1, "req", "success")
		requireExecution(t, s, "waiting")
		if at := executionAtOf(t, s, "one"); at != started {
			t.Fatalf("running -> waiting moved executionAt from %s to %s", started, at)
		}
	})
	t.Run("owner and infrastructure", func(t *testing.T) {
		s := executionStore(t)
		mustStart(t, s, "release")
		started := executionAtOf(t, s, "one")
		before, err := s.GetSwarm(ctx, "one")
		mustNil(t, err)
		seedUser(t, s, "w", "next-owner")
		mustNil(t, s.AddMember(ctx, "one", "w"))
		mustNil(t, s.SetSwarmOwner(ctx, "one", "w"))
		mustNil(t, s.SetSwarmAtespaceUID(ctx, "one", ptr("native-one")))
		mustNil(t, s.SetSwarmProgress(ctx, "one", "active", ""))
		after, err := s.GetSwarm(ctx, "one")
		mustNil(t, err)
		if *after.UpdatedAt == *before.UpdatedAt || after.OwnerID != "w" {
			t.Fatalf("fixture writes did not happen: updated_at %s -> %s, owner %s", *before.UpdatedAt, *after.UpdatedAt, after.OwnerID)
		}
		if at := executionAtOf(t, s, "one"); at != started {
			t.Fatalf("owner and infrastructure writes moved executionAt from %s to %s", started, at)
		}
	})
}

// TestExecutionAtMarksTheStopMidStep: остановка посреди шага актора, чья
// инструкция уже ушла, пишет момент остановки, хотя сам шаг остаётся
// inflight без finished_at: по шагам этот момент не восстановить.
func TestExecutionAtMarksTheStopMidStep(t *testing.T) {
	ctx := context.Background()
	s := executionStore(t)
	mustStart(t, s, "build-first")
	a := attestOne(t, s, "req")
	mustNil(t, s.FinishActorStepDelivery(ctx, a))
	before := time.Now().UTC()
	_, err := s.StopSwarm(ctx, "one", "u", allow)
	mustNil(t, err)
	requireMomentWithin(t, "stop", executionAtOf(t, s, "one"), before, time.Now().UTC())
	if v := stepOf(t, s, 1); v.State != "inflight" || v.FinishedAt != nil {
		t.Fatalf("stopped inflight step: %+v", v)
	}
}

// TestExecutionAtMarksTheEndOfAPass: проход, кончившийся succeeded или
// failed, пишет момент окончания, равный finished_at шага, который его
// кончил: ответа оператора или исхода актора, ведущего в выход, или
// терминального отказа без ребра. Следующий запуск переносит момент на
// начало нового прохода.
func TestExecutionAtMarksTheEndOfAPass(t *testing.T) {
	for _, tc := range []struct {
		name, definition, execution string
		finish                      func(t *testing.T, s *Store)
	}{
		{"answer to exit", "approve", "succeeded", func(t *testing.T, s *Store) {
			mustNil(t, answer(s, openNodeRequestOf(t, s, 1), "u", "success", ""))
		}},
		{"rejection to exit", "release", "failed", func(t *testing.T, s *Store) {
			mustNil(t, answer(s, openNodeRequestOf(t, s, 1), "u", "fail", ""))
		}},
		{"actor to exit", "build-first", "succeeded", func(t *testing.T, s *Store) { completeBuild(t, s, 1, "req", "success") }},
		{"actor failure without an edge", "strict", "failed", func(t *testing.T, s *Store) { completeBuild(t, s, 1, "req", "fail") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := executionStore(t)
			mustStart(t, s, tc.definition)
			before := time.Now().UTC()
			tc.finish(t, s)
			after := time.Now().UTC()
			requireExecution(t, s, tc.execution)
			at := executionAtOf(t, s, "one")
			requireMomentWithin(t, tc.name, at, before, after)
			if finished := stepOf(t, s, 1).FinishedAt; finished == nil || *finished != at {
				t.Fatalf("executionAt %s is not the finished_at %v of the step that ended the pass", at, finished)
			}
			before = time.Now().UTC()
			mustStart(t, s, tc.definition)
			requireMomentWithin(t, "restart", executionAtOf(t, s, "one"), before, time.Now().UTC())
		})
	}
}
