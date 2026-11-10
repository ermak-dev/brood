package store

// FEATURE: Итог предыдущего шага прохода в резерве шага актора (#248).
// SPEC: Резерв шага actor-узла той же транзакцией, что задача прохода,
// читает непосредственно предыдущий шаг этого прохода: у human-узла — его
// отвеченный узловой запрос (вопрос, материал, варианты, исход и текст
// ответа оператора), у actor-узла — его узел, исход и отчёт (#233). Первый
// шаг прохода предыдущего шага не получает: шаги прошлых проходов не
// читаются. Схема не меняется.
// IMPLEMENTATION: ReserveActorStep, previousStep.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"testing"
)

// chainGraph — два actor-узла роли build подряд: исполнитель, затем проверка.
const chainGraph = `apiVersion: brood.dev/v1alpha1
name: chain
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: work}
  verify: {type: actor, role: build, prompt: check}
  done: {type: exit}
edges:
  - {from: build, to: verify}
  - {from: verify, to: done}
goalGates: [done]
`

// answerNodeWith отвечает на открытый узловой запрос шага sequence исходом и
// текстом оператора.
func answerNodeWith(t *testing.T, s *Store, sequence int64, outcome, response string) {
	t.Helper()
	req := openNodeRequestOf(t, s, sequence)
	if _, err := s.AnswerSwarmRequest(context.Background(), SwarmAnswer{SwarmID: "one", RequestID: req.ID, UserID: "u", Outcome: outcome, Response: response}, allow); err != nil {
		t.Fatalf("answer step %d: %v", sequence, err)
	}
}

// chainStore — startStore с актором, готовым к управлению workspace, и
// каталогом с chain.
func chainStore(t *testing.T) *Store {
	t.Helper()
	s := startStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	publish(t, s, "one", map[string]string{"release": startRelease, "approve": startApprove, "build-first": startBuildFirst, "chain": chainGraph})
	return s
}

// TestReserveCarriesOperatorDecision — шаг после human-узла получает его
// отвеченный запрос: вопрос, материал (отчёт шага до узла), выбранный
// вариант и текст ответа оператора. Возврат на доработку несёт решение
// последнего узла, а не первого.
func TestReserveCarriesOperatorDecision(t *testing.T) {
	s := reportStore(t)
	const begin = "Start with the tokenizer."
	answerNodeWith(t, s, 1, "success", begin)
	a := deliverOne(t, s, "req-2")
	p := a.Previous
	if p == nil || p.Sequence != 1 || p.NodeID != "ask" || p.Outcome != "success" || p.Report != "" || p.Request == nil {
		t.Fatalf("previous step of step 2: %+v", p)
	}
	if r := p.Request; r.Question != "Begin?" || r.Context != "" || r.Response == nil || *r.Response != begin {
		t.Fatalf("operator decision of step 1: %+v", r)
	}
	if label, ok := p.Request.OptionLabel(); !ok || label != "Begin" {
		t.Fatalf("chosen option of step 1: %q %v", label, ok)
	}

	const report = "Implemented the parser on branch feat/7-parser.\n\tMerge request !12."
	mustNil(t, completeWithReport(s, a, "success", report))
	const rework = "Add tests for nested comments.\nKeep the public API."
	answerNodeWith(t, s, 3, "fail", rework)
	a = reserveOne(t, s, "req-4")
	p = a.Previous
	if p == nil || p.Sequence != 3 || p.NodeID != "approve" || p.Outcome != "fail" || p.Request == nil {
		t.Fatalf("previous step of step 4: %+v", p)
	}
	r := p.Request
	if r.Question != "Approve the change?" || r.Context != "Report of step 2 (node build, outcome success):\n\n"+report || r.Response == nil || *r.Response != rework {
		t.Fatalf("operator decision of step 3: %+v", r)
	}
	if label, ok := r.OptionLabel(); !ok || label != "Rework" {
		t.Fatalf("chosen option of step 3: %q %v", label, ok)
	}
}

// TestReserveCarriesPreviousReport — шаг после actor-узла получает его узел,
// исход и отчёт; шаг без отчёта — пустой отчёт; первый шаг прохода
// предыдущего шага не получает.
func TestReserveCarriesPreviousReport(t *testing.T) {
	s := chainStore(t)
	mustStart(t, s, "chain")
	a := deliverOne(t, s, "req-1")
	if a.Previous != nil {
		t.Fatalf("first step of the pass carries a previous step: %+v", a.Previous)
	}
	const report = "Built the parser.\nBranch feat/7-parser."
	mustNil(t, completeWithReport(s, a, "success", report))
	a = reserveOne(t, s, "req-2")
	if p := a.Previous; p == nil || p.Sequence != 1 || p.NodeID != "build" || p.Outcome != "success" || p.Report != report || p.Request != nil {
		t.Fatalf("previous step of step 2: %+v", p)
	}

	s = chainStore(t)
	mustStart(t, s, "chain")
	mustNil(t, completeWithReport(s, deliverOne(t, s, "req-1"), "success", ""))
	if p := reserveOne(t, s, "req-2").Previous; p == nil || p.Sequence != 1 || p.Report != "" || p.Outcome != "success" {
		t.Fatalf("previous step without a report: %+v", p)
	}
}

// TestReserveSeesOnlyItsPass — первый шаг нового прохода не видит шагов
// прошлого, хотя шаг роя перед ним есть и оставил отчёт; второй шаг нового
// прохода видит первый шаг своего прохода.
func TestReserveSeesOnlyItsPass(t *testing.T) {
	s := chainStore(t)
	mustStart(t, s, "chain")
	const first, second = "First pass: built the parser.", "Second pass: built the lexer."
	mustNil(t, completeWithReport(s, deliverOne(t, s, "req-1"), "success", first))
	mustNil(t, completeWithReport(s, deliverOne(t, s, "req-2"), "success", first))
	// Шаг 3 — выход первого прохода; новый проход начинается с шага 4.
	if v := stepOf(t, s, 3); v.NodeID != "done" {
		t.Fatalf("last step of the first pass: %+v", v)
	}
	mustStart(t, s, "chain")
	a := deliverOne(t, s, "req-4")
	if a.Sequence != 4 || a.Pass != 4 || a.Previous != nil {
		t.Fatalf("first step of the new pass: sequence %d pass %d previous %+v", a.Sequence, a.Pass, a.Previous)
	}
	mustNil(t, completeWithReport(s, a, "success", second))
	if p := reserveOne(t, s, "req-5").Previous; p == nil || p.Sequence != 4 || p.Report != second {
		t.Fatalf("previous step of step 5: %+v", p)
	}

	s = reportStore(t)
	answerNodeWith(t, s, 1, "success", "Begin with the first pass.")
	stopOne(t, s)
	mustStart(t, s, "reviewed")
	if v := stepOf(t, s, 3); v.NodeID != "ask" || v.Pass != 3 {
		t.Fatalf("first step of the new pass: %+v", v)
	}
	answerNodeWith(t, s, 3, "success", "Begin with the second pass.")
	if p := reserveOne(t, s, "req-4").Previous; p == nil || p.Sequence != 3 || p.Request == nil || *p.Request.Response != "Begin with the second pass." {
		t.Fatalf("previous step of step 4: %+v", p)
	}
}
