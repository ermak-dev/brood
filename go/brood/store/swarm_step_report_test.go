package store

// FEATURE: Отчёт актора о шаге и материал узлового запроса (#233).
// SPEC: Актор, завершая шаг, оставляет отчёт: что сделано и где результат.
// Отчёт хранится на шаге и пишется один раз вместе с исходом; точный повтор
// той же попытки ничего не пишет, другой отчёт к завершённому шагу —
// конфликт. Отчёт от актора, не привязанного к роли шага, отвергается тем же
// отказом, что исход. Узловой запрос human-узла получает в context отчёт
// шага, который ввёл граф в этот узел в этом проходе, с номером шага;
// отчёты прошлых проходов и более ранних шагов в него не попадают.
// IMPLEMENTATION: CompletePipelineActorVisit, progressPipeline,
// openNodeRequest; колонка report и триггер swarm_steps_report_fence.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// reportGraph — оператор открывает проход, актор роли build работает,
// оператор принимает результат или возвращает его на доработку. Отказ
// актора безусловное ребро не берёт: проход завершается неуспехом.
const reportGraph = `apiVersion: brood.dev/v1alpha1
name: reviewed
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Begin?
    options:
      - {label: Begin, outcome: success}
  build: {type: actor, role: build, prompt: work}
  approve:
    type: human
    question: Approve the change?
    options:
      - {label: Approve, outcome: success}
      - {label: Rework, outcome: fail}
  done: {type: exit}
edges:
  - {from: ask, to: build}
  - {from: build, to: approve}
  - {from: approve, to: done, label: Approve}
  - {from: approve, to: build, label: Rework}
goalGates: [done]
`

// reportStore — startStore с актором, готовым к управлению workspace, и
// запущенным reviewed: первый шаг ask ждёт оператора.
func reportStore(t *testing.T) *Store {
	t.Helper()
	s := startStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	if _, err := s.ReserveManagedActor(context.Background(), managedRequest("b", "one", 202)); err != nil {
		t.Fatal(err)
	}
	setActor(t, s, "b", "phase='running', stage='done', operation='start'")
	publish(t, s, "one", map[string]string{"release": startRelease, "approve": startApprove, "build-first": startBuildFirst, "reviewed": reportGraph})
	mustStart(t, s, "reviewed")
	return s
}

// answerNode отвечает на открытый узловой запрос шага sequence.
func answerNode(t *testing.T, s *Store, sequence int64, outcome string) {
	t.Helper()
	req := openNodeRequestOf(t, s, sequence)
	if _, err := s.AnswerSwarmRequest(context.Background(), SwarmAnswer{SwarmID: "one", RequestID: req.ID, UserID: "u", Outcome: outcome}, allow); err != nil {
		t.Fatalf("answer step %d: %v", sequence, err)
	}
}

// deliverOne доводит ждущий шаг актора до inflight: резерв, подтверждение,
// отпущенный workspace-lease и записанная доставка.
func deliverOne(t *testing.T, s *Store, requestID string) ActorStepAttempt {
	t.Helper()
	ctx := context.Background()
	a := attestOne(t, s, requestID)
	mustNil(t, s.FinishActorOperation(ctx, a.Lease, true))
	mustNil(t, s.FinishActorStepDelivery(ctx, a))
	return a
}

func completeWithReport(s *Store, a ActorStepAttempt, outcome, report string) error {
	return s.CompletePipelineActorVisit(context.Background(), "one", "one", a.Sequence, a.RequestID, a.Actor.UID, a.Actor.Incarnation, outcome, report)
}

// TestActorStepReportKeptOnce — отчёт пишется вместе с исходом; точный
// повтор ничего не пишет, другой или пропущенный отчёт к завершённому шагу —
// конфликт без записи; схема не даёт переписать отчёт мимо кода.
func TestActorStepReportKeptOnce(t *testing.T) {
	s := reportStore(t)
	answerNode(t, s, 1, "success")
	a := deliverOne(t, s, "req-2")
	const report = "Implemented the parser.\n\tBranch feat/7-parser, merge request !12, notes in docs/PARSER.md."
	mustNil(t, completeWithReport(s, a, "success", report))
	if v := stepOf(t, s, 2); v.State != "completed" || v.Report != report {
		t.Fatalf("reported step: %+v", v)
	}
	before := dumpTables(t, s, executionTables...)
	mustNil(t, completeWithReport(s, a, "success", report))
	for name, retry := range map[string]func() error{
		"other report": func() error { return completeWithReport(s, a, "success", "Rewritten after the fact.") },
		"no report":    func() error { return completeWithReport(s, a, "success", "") },
		"other outcome": func() error {
			return completeWithReport(s, a, "fail", report)
		},
	} {
		if err := retry(); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("%s for a finished step: %v", name, err)
		}
	}
	if after := dumpTables(t, s, executionTables...); after != before {
		t.Fatalf("retry wrote:\n%s\nwant\n%s", after, before)
	}
	for _, statement := range []string{
		`UPDATE swarm_steps SET report='forged' WHERE swarm_id='one' AND sequence=2`,
		`UPDATE swarm_steps SET report=NULL WHERE swarm_id='one' AND sequence=2`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("%s accepted", statement)
		}
	}
	if v := stepOf(t, s, 2); v.Report != report {
		t.Fatalf("report changed: %q", v.Report)
	}
}

// TestActorStepReportFromUnboundActor — отчёт актора, не привязанного к
// роли шага, или другой инкарнации отвергается тем же отказом, что исход:
// ни отчёта, ни исхода, ни узлового запроса.
func TestActorStepReportFromUnboundActor(t *testing.T) {
	ctx := context.Background()
	s := reportStore(t)
	answerNode(t, s, 1, "success")
	a := deliverOne(t, s, "req-2")
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "b"}))
	before := dumpTables(t, s, executionTables...)
	for _, who := range []struct {
		uid         string
		incarnation int64
	}{{"a", 1}, {"b", 1}, {"a", 2}} {
		err := s.CompletePipelineActorVisit(ctx, "one", "one", a.Sequence, a.RequestID, who.uid, who.incarnation, "success", "Done by someone else.")
		if !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("report from %s/%d: %v", who.uid, who.incarnation, err)
		}
	}
	if after := dumpTables(t, s, executionTables...); after != before {
		t.Fatalf("refused report wrote:\n%s\nwant\n%s", after, before)
	}
	if v := stepOf(t, s, 2); v.State != "inflight" || v.Report != "" {
		t.Fatalf("refused report changed the step: %+v", v)
	}
}

// TestActorStepReportValidation — отчёт — текст до MaxStepReportBytes байт
// валидного UTF-8 без управляющих символов, кроме перевода строки и
// табуляции. Неверный отчёт отвергается до записи, и исход вместе с ним.
func TestActorStepReportValidation(t *testing.T) {
	s := reportStore(t)
	answerNode(t, s, 1, "success")
	a := deliverOne(t, s, "req-2")
	before := dumpTables(t, s, executionTables...)
	for name, report := range map[string]string{
		"escape":         "ok \x1b[31mred",
		"carriage":       "line\rover",
		"nul":            "a\x00b",
		"delete":         "a\x7fb",
		"c1 control":     "a\u0085b",
		"invalid utf-8":  "a\xffb",
		"oversized":      strings.Repeat("x", MaxStepReportBytes+1),
		"oversized utf8": strings.Repeat("ж", MaxStepReportBytes/2) + "x",
	} {
		if err := completeWithReport(s, a, "success", report); !errors.Is(err, ErrStepReportInvalid) {
			t.Fatalf("%s report: %v", name, err)
		}
	}
	if after := dumpTables(t, s, executionTables...); after != before {
		t.Fatalf("invalid report wrote:\n%s\nwant\n%s", after, before)
	}
	limit := strings.Repeat("ж", MaxStepReportBytes/2-1) + "\n\t"
	mustNil(t, completeWithReport(s, a, "success", limit))
	if v := stepOf(t, s, 2); v.Report != limit {
		t.Fatalf("report at the limit: %d bytes stored", len(v.Report))
	}
}

// TestNodeRequestContextIsPredecessorReport — материал узлового запроса —
// отчёт шага, который ввёл граф в human-узел в этом проходе, с номером шага.
// Отчёт более раннего шага или прошлого прохода в него не попадает; шаг без
// отчёта и первый шаг прохода материала не дают.
func TestNodeRequestContextIsPredecessorReport(t *testing.T) {
	s := reportStore(t)
	if req := openNodeRequestOf(t, s, 1); req.Context != "" {
		t.Fatalf("first gate context: %q", req.Context)
	}
	answerNode(t, s, 1, "success")
	mustNil(t, completeWithReport(s, deliverOne(t, s, "req-2"), "success", "First draft on branch feat/1-draft."))
	first := openNodeRequestOf(t, s, 3)
	for _, part := range []string{"step 2", "build", "success", "First draft on branch feat/1-draft."} {
		if !strings.Contains(first.Context, part) {
			t.Fatalf("gate context lacks %q: %q", part, first.Context)
		}
	}

	answerNode(t, s, 3, "fail")
	mustNil(t, completeWithReport(s, deliverOne(t, s, "req-4"), "success", "Second draft, merge request !4."))
	second := openNodeRequestOf(t, s, 5)
	if !strings.Contains(second.Context, "step 4") || !strings.Contains(second.Context, "Second draft, merge request !4.") || strings.Contains(second.Context, "First draft") || strings.Contains(second.Context, "step 2") {
		t.Fatalf("second gate context: %q", second.Context)
	}

	// Отказ актора завершает проход на его шаге: отчёт остаётся последним.
	answerNode(t, s, 5, "fail")
	mustNil(t, completeWithReport(s, deliverOne(t, s, "req-6"), "fail", "Could not build: the toolchain is missing."))
	if w, err := s.GetSwarm(context.Background(), "one"); err != nil || w.Execution != "failed" {
		t.Fatalf("failed pass: %+v %v", w, err)
	}

	// Новый проход: шаг перед его первым узлом — из прошлого прохода.
	mustStart(t, s, "reviewed")
	if req := openNodeRequestOf(t, s, 7); req.Context != "" {
		t.Fatalf("next pass gate carries the previous pass report: %q", req.Context)
	}
	answerNode(t, s, 7, "success")
	mustNil(t, completeWithReport(s, deliverOne(t, s, "req-8"), "success", ""))
	if req := openNodeRequestOf(t, s, 9); req.Context != "" {
		t.Fatalf("gate after a step without a report: %q", req.Context)
	}
}

// TestSwarmStepSchemaHoldsReport — схема держит предел отчёта и правило:
// отчёт есть только у завершённого шага и пишется с переходом в completed.
func TestSwarmStepSchemaHoldsReport(t *testing.T) {
	for _, tc := range []struct {
		name, statement string
		completed       bool
	}{
		{"oversized", `UPDATE swarm_steps SET state='completed',outcome='success',report='` + strings.Repeat("x", MaxStepReportBytes+1) + `' WHERE swarm_id='one' AND sequence=2`, false},
		{"empty", `UPDATE swarm_steps SET state='completed',outcome='success',report='' WHERE swarm_id='one' AND sequence=2`, false},
		{"before completion", `UPDATE swarm_steps SET report='early' WHERE swarm_id='one' AND sequence=2`, false},
		{"answered step", `UPDATE swarm_steps SET report='early' WHERE swarm_id='one' AND sequence=1`, false},
		{"inserted", `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,report) VALUES ('one',9,'build','pending','early')`, false},
		{"after completion", `UPDATE swarm_steps SET report='late' WHERE swarm_id='one' AND sequence=2`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reportStore(t)
			answerNode(t, s, 1, "success")
			a := deliverOne(t, s, "req-2")
			if tc.completed {
				mustNil(t, completeWithReport(s, a, "success", ""))
			}
			if _, err := s.db.Exec(tc.statement); err == nil {
				t.Fatalf("%s accepted", tc.name)
			}
		})
	}
}
