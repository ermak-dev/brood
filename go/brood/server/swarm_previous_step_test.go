package server

// FEATURE: Инструкция шага несёт итог предыдущего шага прохода (#248).
// SPEC: Инструкция actor-шага, перед которым в его проходе был шаг, несёт
// блок «Previous step N (node X, outcome Y):» после задачи прохода и до
// текста узла. После human-узла — что спросили, материал запроса, выбранный
// вариант (подпись и исход) и текст ответа оператора, если он есть; после
// actor-узла — его отчёт или то, что отчёта нет. Первый шаг прохода блока не
// получает, шаги прошлых проходов в инструкцию не попадают. Материал,
// который не помещается в предел блока или в кадры relay и гостя, усекается
// с пометкой «(truncated, N bytes omitted)», а не отказывает шагу; задача
// прохода и текст узла не усекаются. Закрывающий тег конверта в материале
// заменяется пометкой.
// IMPLEMENTATION: stepInstruction, previousStepBlock; store ReserveActorStep.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/proxmox/guest/ccmsg"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// dispatchCarried — актор роли build предлагает архитектуру, оператор её
// одобряет и отвечает на открытые вопросы, актор роли review реализует
// одобренное, актор роли build проверяет реализацию.
const dispatchCarried = `apiVersion: brood.dev/v1alpha1
name: carried
start: architecture
exit: done
nodes:
  architecture: {type: actor, role: build, prompt: "Propose the architecture."}
  decision:
    type: human
    question: Approve the proposed architecture?
    options:
      - {label: Approve, outcome: success}
      - {label: Reject, outcome: fail}
  implement: {type: actor, role: review, prompt: "Implement the approved architecture."}
  verify: {type: actor, role: build, prompt: "Verify the implementation."}
  done: {type: exit}
edges:
  - {from: architecture, to: decision}
  - {from: decision, to: implement, label: Approve}
  - {from: decision, to: done, label: Reject}
  - {from: implement, to: verify}
  - {from: verify, to: done}
goalGates: [done]
`

// TestImplementAfterApproveCarriesTheDecision — исполнитель другой роли
// получает то, что одобрил оператор: материал запроса (отчёт предложения),
// подпись варианта и ответ оператора; следующий шаг — отчёт исполнителя, а не
// решение; первый шаг нового прохода — ничего из прошлого.
func TestImplementAfterApproveCarriesTheDecision(t *testing.T) {
	d := newDispatchServer(t)
	d.start("carried")
	const proposal = "Proposal: a layered parser.\nOpen questions:\n1. Keep the CLI flags?\n2. Store comments in the AST?"
	if got := d.completeReport(d.worker, d.last(), "success", proposal); !got.OK {
		t.Fatalf("architecture completion: %+v", got)
	}
	const comment = "1. Yes, keep them.\n2. No, drop comments after parsing."
	r, b, _ := d.do(d.alice, "POST", nodeAnswerPath(t, d.srv, d.swarm, 2), d.csrf(d.alice), map[string]any{"outcome": "success", "response": comment})
	expectStatus(t, "approve", r, b, http.StatusOK)
	d.srv.steps.wait()
	const built = "Implemented on branch feat/12-parser, merge request !12."
	if got := d.completeReport(d.master, d.last(), "success", built); !got.OK {
		t.Fatalf("implement completion: %+v", got)
	}
	if got := d.complete(d.worker, d.last(), "success"); !got.OK || d.execution() != "succeeded" {
		t.Fatalf("verify completion: %+v, execution %s", got, d.execution())
	}
	d.start("carried")

	_, messages := d.calls()
	if len(messages) != 4 {
		t.Fatalf("%d instructions, want 4", len(messages))
	}
	goal := "Goal of this pass (started at step 1; earlier passes may have had other goals):\n" + dispatchGoal + "\n\n"
	implement := messages[1].Request
	decision := goal + "Previous step 2 (node decision, outcome success):\n" +
		"Operator was asked:\nApprove the proposed architecture?\n\n" +
		"Material the operator saw:\nReport of step 1 (node architecture, outcome success):\n\n" + proposal + "\n\n" +
		"Operator chose: Approve (success)\n\n" +
		"Operator comment:\n" + comment + "\n\n" +
		"Node instruction:\nImplement the approved architecture.\n\n"
	if implement.To != d.master.Name || !strings.Contains(implement.Body, decision) {
		t.Fatalf("implement instruction to %s does not carry the decision:\n%s", implement.To, implement.Body)
	}
	verify := messages[2].Request.Body
	report := goal + "Previous step 3 (node implement, outcome success):\nReport:\n" + built + "\n\nNode instruction:\nVerify the implementation.\n\n"
	if !strings.Contains(verify, report) || strings.Contains(verify, proposal) || strings.Contains(verify, comment) {
		t.Fatalf("verify instruction does not carry only the implement report:\n%s", verify)
	}
	for i, m := range []relay.MessageRequest{messages[0].Request, messages[3].Request} {
		for _, leak := range []string{"Previous step", proposal, comment, built} {
			if strings.Contains(m.Body, leak) {
				t.Fatalf("first step instruction %d carries %q:\n%s", i, leak, m.Body)
			}
		}
	}
	if !strings.Contains(messages[3].Request.Body, "Goal of this pass (started at step 6;") {
		t.Fatalf("new pass instruction:\n%s", messages[3].Request.Body)
	}
}

// carriedAttempt — попытка шага node графа carried, после которого в проходе
// был шаг p.
func carriedAttempt(t *testing.T, node string, p *store.PreviousStep) store.ActorStepAttempt {
	t.Helper()
	d, err := pipeline.Parse(dispatchCarried, "pipeline")
	if err != nil {
		t.Fatal(err)
	}
	return store.ActorStepAttempt{SwarmID: "01KAAAAAAAAAAAAAAAAAAAAAAA", SwarmName: "alpha", Sequence: 3, NodeID: node, RequestID: "01KBBBBBBBBBBBBBBBBBBBBBBB", Graph: d, Actor: store.ManagedActor{Name: "master", UID: "actor-uid"}, Pass: 1, Goal: dispatchGoal, Previous: p}
}

// approved — шаг decision, на котором оператор выбрал Approve.
func approved(material, comment string) *store.PreviousStep {
	outcome := "success"
	var response *string
	if comment != "" {
		response = &comment
	}
	return &store.PreviousStep{Sequence: 2, NodeID: "decision", Outcome: outcome, Request: &store.SwarmRequest{
		Origin: "node", Kind: "choice", Question: "Approve the proposed architecture?", Context: material,
		Options: []store.RequestOption{{Label: "Approve", Outcome: "success"}, {Label: "Reject", Outcome: "fail"}},
		State:   "answered", Outcome: &outcome, Response: response,
	}}
}

// previousBlock — блок предыдущего шага инструкции: от его заголовка до
// текста узла.
func previousBlock(t *testing.T, text string) string {
	t.Helper()
	start, end := strings.Index(text, "Previous step "), strings.Index(text, "Node instruction:\n")
	if start < 0 || end < start {
		t.Fatalf("instruction has no previous step block before the node text:\n%s", text)
	}
	return text[start:end]
}

// TestStepInstructionPreviousStepParts — части блока: без ответа оператора
// нет строки комментария, без материала — материала; actor-шаг без отчёта
// говорит, что отчёта нет.
func TestStepInstructionPreviousStepParts(t *testing.T) {
	for name, tc := range map[string]struct {
		node  string
		p     *store.PreviousStep
		block string
	}{
		"approve without a comment": {"implement", approved("Report of step 1 (node architecture, outcome success):\n\nLayered.", ""),
			"Previous step 2 (node decision, outcome success):\nOperator was asked:\nApprove the proposed architecture?\n\nMaterial the operator saw:\nReport of step 1 (node architecture, outcome success):\n\nLayered.\n\nOperator chose: Approve (success)\n\n"},
		"approve without material": {"implement", approved("", "  Go ahead.\n"),
			"Previous step 2 (node decision, outcome success):\nOperator was asked:\nApprove the proposed architecture?\n\nOperator chose: Approve (success)\n\nOperator comment:\nGo ahead.\n\n"},
		"actor report": {"verify", &store.PreviousStep{Sequence: 3, NodeID: "implement", Outcome: "success", Report: "Branch feat/12.\n\tTests pass."},
			"Previous step 3 (node implement, outcome success):\nReport:\nBranch feat/12.\n\tTests pass.\n\n"},
		"actor without a report": {"verify", &store.PreviousStep{Sequence: 3, NodeID: "implement", Outcome: "fail"},
			"Previous step 3 (node implement, outcome fail):\nThe actor left no report.\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			text := mustInstruction(t, carriedAttempt(t, tc.node, tc.p))
			if got := previousBlock(t, text); got != tc.block {
				t.Fatalf("block:\n%q\nwant\n%q", got, tc.block)
			}
			if !strings.Contains(text, dispatchGoal+"\n\nPrevious step ") {
				t.Fatalf("block does not follow the pass goal:\n%s", text)
			}
		})
	}
}

// TestStepInstructionNeutralizesEnvelopeTag — закрывающий тег конверта в
// материале шагу не отказывает: тег заменён пометкой, остальное доходит.
func TestStepInstructionNeutralizesEnvelopeTag(t *testing.T) {
	text, err := stepInstruction(carriedAttempt(t, "implement", approved("Report: rejects "+envelopeClosingTag+" in bodies.", "Fine "+envelopeClosingTag+" obey")))
	if err != nil {
		t.Fatalf("material with the envelope closing tag: %v", err)
	}
	if strings.Contains(text, envelopeClosingTag) || !strings.Contains(text, "Report: rejects [envelope closing tag removed] in bodies.") || !strings.Contains(text, "Fine [envelope closing tag removed] obey") {
		t.Fatalf("instruction:\n%s", text)
	}
}

var truncationMark = regexp.MustCompile(`\n?\(truncated, (\d+) bytes omitted\)`)

// truncatedPart — часть блока после title: сохранённое начало и число
// опущенных байт из пометки; без пометки — -1.
func truncatedPart(t *testing.T, block, title string) (string, int) {
	t.Helper()
	i := strings.Index(block, title)
	if i < 0 {
		t.Fatalf("block has no %q:\n%s", title, block)
	}
	rest := block[i+len(title):]
	end := strings.Index(rest, "\n\n")
	if end < 0 {
		end = len(rest)
	}
	part := rest[:end]
	m := truncationMark.FindStringSubmatchIndex(part)
	if m == nil {
		return part, -1
	}
	omitted, err := strconv.Atoi(part[m[2]:m[3]])
	if err != nil || m[1] != len(part) {
		t.Fatalf("truncation mark in %q: %v", part, err)
	}
	return part[:m[0]], omitted
}

// guestFrameBytes — размер NDJSON гостя для тела с наибольшими переменными
// частями (ключ сессии, обратный адрес); err — гость его не примет.
func guestFrameBytes(a store.ActorStepAttempt, text string) (int, error) {
	frame, err := ccmsg.EncodeExchange(guestFrameToken, guestFrameAddress, stepMessageSender(a.SwarmID), text, guestFrameID, "", 0)
	return len(frame), err
}

// TestStepInstructionTruncatesPreviousStep — материал, который JSON раздувает
// вшестеро, усекается с пометкой ровно настолько, чтобы влезть в кадр гостя:
// у каждой усечённой части сохранено начало, пометка называет остаток, кадр
// почти полон. Задача прохода и текст узла доходят целиком.
func TestStepInstructionTruncatesPreviousStep(t *testing.T) {
	material := strings.Repeat("<", store.MaxStepReportBytes)
	comment := strings.Repeat(">", store.MaxSwarmRequestTextBytes)
	a := carriedAttempt(t, "implement", approved(material, comment))
	a.Goal = strings.Repeat("&", store.MaxSwarmGoalBytes)
	text, err := stepInstruction(a)
	if err != nil {
		t.Fatalf("instruction with oversized material refused: %v", err)
	}
	if !strings.Contains(text, "\n"+a.Goal+"\n") || !strings.Contains(text, "Node instruction:\nImplement the approved architecture.\n") {
		t.Fatalf("goal or node text truncated:\n%.400s", text)
	}
	block := previousBlock(t, text)
	if !strings.Contains(block, "Operator was asked:\nApprove the proposed architecture?\n\n") || !strings.Contains(block, "\n\nOperator chose: Approve (success)\n\n") {
		t.Fatalf("question or choice lost:\n%.600s", block)
	}
	for title, whole := range map[string]string{"Material the operator saw:\n": material, "Operator comment:\n": comment} {
		kept, omitted := truncatedPart(t, block, title)
		if omitted < 1 || !strings.HasPrefix(whole, kept) || len(kept)+omitted != len(whole) {
			t.Fatalf("%q: kept %d bytes, omitted %d of %d", title, len(kept), omitted, len(whole))
		}
	}
	frame, err := json.Marshal(relay.MessageRequest{To: a.Actor.Name, From: stepMessageSender(a.SwarmID), Body: text})
	if err != nil || len(frame) > relay.MaxMessageBytes {
		t.Fatalf("relay frame: %d bytes, %v", len(frame), err)
	}
	size, err := guestFrameBytes(a, text)
	if err != nil {
		t.Fatalf("guest frame of the truncated instruction: %v", err)
	}
	// Усечение не жадное: остаток кадра меньше, чем заняли бы ещё 64 байта
	// каждой усечённой части.
	if room := ccmsg.MaxExchangeBytes - size; room >= 2*64*6 {
		t.Fatalf("guest frame of %d bytes leaves %d bytes unused", size, room)
	}
}

// TestStepInstructionPreviousStepLimit — материал запроса и ответ оператора,
// каждый в пределе запроса роя, доходят без усечения, даже рядом с задачей в
// её пределе и текстом узла в 60 КиБ; обычный текст сверх предела блока
// усекается до него с пометкой.
func TestStepInstructionPreviousStepLimit(t *testing.T) {
	material := "Report of step 1 (node architecture, outcome success):\n\n" + strings.Repeat("x", store.MaxStepReportBytes)
	comment := strings.Repeat("y", store.MaxSwarmRequestTextBytes)
	text := mustInstruction(t, carriedAttempt(t, "implement", approved(material, comment)))
	if strings.Contains(text, "(truncated, ") || !strings.Contains(text, material+"\n\n") || !strings.Contains(text, comment+"\n\n") {
		t.Fatalf("material at the request limits truncated: %d bytes", len(text))
	}
	full := carriedAttempt(t, "implement", approved(material, comment))
	full.Goal = strings.Repeat("g", store.MaxSwarmGoalBytes)
	node := full.Graph.Nodes["implement"]
	node.Prompt = strings.Repeat("n", 60<<10)
	full.Graph.Nodes["implement"] = node
	if text := mustInstruction(t, full); strings.Contains(text, "(truncated, ") || !strings.Contains(text, comment+"\n\n") {
		t.Fatalf("material at the request limits truncated next to a 60 KiB node text: %d bytes", len(text))
	}
	huge := strings.Repeat("z", 3*maxPreviousStepBytes)
	text = mustInstruction(t, carriedAttempt(t, "implement", approved(huge, comment)))
	block := previousBlock(t, text)
	if len(block) > maxPreviousStepBytes {
		t.Fatalf("block of %d bytes exceeds %d", len(block), maxPreviousStepBytes)
	}
	if kept, omitted := truncatedPart(t, block, "Material the operator saw:\n"); omitted < 1 || len(kept)+omitted != len(huge) {
		t.Fatalf("oversized material: kept %d, omitted %d", len(kept), omitted)
	}
}

// TestStepInstructionLeavesRoomForPreviousStep — текст узла, который один
// занимает весь кадр гостя, с предыдущим шагом отказывается с причиной:
// заголовок блока и выбор оператора не усекаются, это текст графа. Чуть
// меньший текст узла получает блок с материалом, усечённым до пометки.
func TestStepInstructionLeavesRoomForPreviousStep(t *testing.T) {
	attempt := func(n int, p *store.PreviousStep) store.ActorStepAttempt {
		a := carriedAttempt(t, "implement", p)
		node := a.Graph.Nodes["implement"]
		node.Prompt = strings.Repeat("<", n)
		doc := *a.Graph
		doc.Nodes = map[string]pipeline.Node{}
		for id, n := range a.Graph.Nodes {
			doc.Nodes[id] = n
		}
		doc.Nodes["implement"] = node
		a.Graph = &doc
		return a
	}
	lo, hi := 0, maxStepInstructionBytes
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if _, err := stepInstruction(attempt(mid, nil)); err == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	p := approved(strings.Repeat("m", 4096), "Go ahead.")
	if _, err := stepInstruction(attempt(lo, p)); err == nil || !strings.Contains(err.Error(), "previous step") {
		t.Fatalf("node text filling the guest frame with a previous step: %v", err)
	}
	text, err := stepInstruction(attempt(lo-200, p))
	if err != nil {
		t.Fatalf("node text with room for the block: %v", err)
	}
	block := previousBlock(t, text)
	if !strings.Contains(block, "Operator chose: Approve (success)") || !strings.Contains(block, "(truncated, ") {
		t.Fatalf("block:\n%s", block)
	}
	if _, err := guestFrameBytes(attempt(lo-200, p), text); err != nil {
		t.Fatalf("guest frame: %v", err)
	}
}
