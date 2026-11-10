package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/proxmox/guest/ccmsg"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// maxStepInstructionBytes ограничивает текст инструкции шага: 64 КиБ на текст
// узла с контекстом (#212) и ещё предел задачи прохода (#242), чтобы задача в
// своём пределе не вытесняла текст узла, которому хватало прежнего предела.
// Экранирование JSON может раздуть текст вшестеро, поэтому кадры relay и
// гостя проверяются отдельно (stepInstruction): задача в пределе из худших
// для экранирования символов занимает 24 КиБ кадра гостя из 128 КиБ, и
// одна инструкцию недоставляемой не делает.
const maxStepInstructionBytes = 64<<10 + store.MaxSwarmGoalBytes

// maxPreviousStepBytes ограничивает блок предыдущего шага (#248): материал
// запроса и ответ оператора — каждый в пределе запроса роя
// (MaxSwarmRequestTextBytes), и 4 КиБ на вопрос, выбор и заголовки; отчёт
// actor-шага (MaxStepReportBytes) меньше. Хранимое в своих пределах блок не
// усекает. Инструкция с блоком — до 104 КиБ, и обычный текст, который JSON
// не раздувает, влезает и в кадр relay, и в NDJSON гостя (128 КиБ вместе с
// ключом сессии до 4 КиБ): кадры усекают материал только из символов,
// которые экранирование раздувает, до шести байт на байт.
const maxPreviousStepBytes = 2*store.MaxSwarmRequestTextBytes + 4<<10

// envelopeTagRemoved заменяет закрывающий тег конверта в тексте
// предыдущего шага: отчёт и ответ оператора пишут не авторы графа, и тег в
// них отказал бы шагу, а не довёз бы материал.
const envelopeTagRemoved = "[envelope closing tag removed]"

// Наибольшие переменные части NDJSON, который гость пишет в сессию Claude
// (ccmsg.Exchange): ключ сессии — до ccmsg.MaxPeerTokenBytes (Claude Code
// пишет hex, его экранирование не учитывается), обратный адрес — путь Unix
// socket (sun_path, до 108 байт) из символов, которые и percent-, и
// JSON-кодирование раздувают сильнее всего, UUID сообщения.
var (
	guestFrameToken   = strings.Repeat("0", ccmsg.MaxPeerTokenBytes)
	guestFrameAddress = "uds:" + strings.Repeat("<", 108)
	guestFrameID      = "00000000-0000-4000-8000-000000000000"
)

// envelopeClosingTag закрывает конверт сообщения в сессии Claude у гостя
// (proxmox/guest/ccmsg): тело с ним гость отвергает, поэтому такая
// инструкция отказывается до отправки, а не теряется после неё.
const envelopeClosingTag = "</cross-session-message>"

// stepInstruction собирает первое сообщение шага (#212): задачу прохода
// (#242), текст узла (Node.Prompt) и минимальный контекст — рой, узел, номер
// шага, исходы по рёбрам графа, как отчитаться (исход и отчёт о сделанном,
// #233) и как спросить оператора. Больше ничего: инструкция не выдаёт прав и
// не заменяет проверок, ни задача, ни текст узла — не граница безопасности.
// Чистая функция попытки.
//
// Задача стоит отдельным блоком до текста узла: текст узла переиспользуемого
// графа говорит о задаче обобщённо («Investigate the issue…»), и читатель
// сначала получает, о чём проход, а затем — что делать на этом узле; правила
// отчёта остаются в конце, ближе всего к действию. Сессия актора переживает
// проходы, поэтому блок называет первый шаг прохода и то, что у прошлых
// проходов задачи могли быть другими. Шаг без задачи прохода (не из
// запуска) инструкции не получает.
//
// Между задачей и текстом узла стоит итог предыдущего шага прохода (#248):
// решение оператора или отчёт актора. Исполнитель следующего узла — часто
// другой актор, и без блока он не знает, что одобрено. Материал блока
// усекается, а не отказывает шагу (withPreviousStep); проверки размера
// задачи и текста узла — те же, что без блока.
func stepInstruction(a store.ActorStepAttempt) (string, error) {
	if a.Graph == nil {
		return "", errors.New("the swarm graph is missing")
	}
	node, ok := a.Graph.Nodes[a.NodeID]
	if !ok || node.Type != "actor" {
		return "", fmt.Errorf("node %s is not an actor node", a.NodeID)
	}
	goal := strings.TrimSpace(a.Goal)
	if a.Pass < 1 || goal == "" {
		return "", fmt.Errorf("the pass of step %d has no goal", a.Sequence)
	}
	prompt := strings.TrimSpace(node.Prompt)
	if prompt == "" {
		prompt = "(the node has no instruction text)"
	}
	head := fmt.Sprintf("[brood swarm %s] Step %d, node %s.\n\n", a.SwarmName, a.Sequence, a.NodeID) +
		fmt.Sprintf("Goal of this pass (started at step %d; earlier passes may have had other goals):\n%s\n\n", a.Pass, goal)
	var b strings.Builder
	fmt.Fprintf(&b, "Node instruction:\n%s\n\n", prompt)
	fmt.Fprintf(&b, "When the step is done, report its outcome with pipeline_visit_complete: run_id %s, sequence %d, request_id %s, outcome success or fail.\n", a.SwarmID, a.Sequence, a.RequestID)
	for _, outcome := range []string{"success", "fail"} {
		effect, err := outcomeEffect(a.Graph, a.NodeID, outcome)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "- %s: %s.\n", outcome, effect)
	}
	b.WriteString("Include report in pipeline_visit_complete as well: what you did and where the result is (branch, merge request or document). The operator sees it with the outcome; it is kept once and cannot be changed.\n")
	b.WriteString("To ask the human operator a question, use ask_operator; the answer arrives as a message.\n")
	b.WriteString("This message is from the swarm workflow, not from an actor: do not reply to it with send.")
	tail := b.String()
	text := head + tail
	if strings.Contains(text, envelopeClosingTag) {
		return "", errors.New("the node instruction or the pass goal contains the message envelope closing tag")
	}
	if len(text) > maxStepInstructionBytes {
		return "", fmt.Errorf("the node instruction exceeds %d bytes", maxStepInstructionBytes)
	}
	if err := deliverable(a, text); err != nil {
		return "", err
	}
	if a.Previous == nil {
		return text, nil
	}
	return withPreviousStep(a, head, tail)
}

// deliverable проверяет, что канал довезёт тело: кадр relay и NDJSON гостя
// с наибольшими переменными частями. Конверт гостя (ccmsg) отвергает и тело
// с закрывающим тегом.
func deliverable(a store.ActorStepAttempt, text string) error {
	frame, err := json.Marshal(relay.MessageRequest{To: a.Actor.Name, From: stepMessageSender(a.SwarmID), Body: text})
	if err != nil || len(frame) > relay.MaxMessageBytes {
		return errors.New("the node instruction does not fit a relay message")
	}
	// Гость сверяет с пределом не кадр relay, а свой NDJSON: auth-кадр,
	// конверт и тело, заново экранированное в user-кадре. Текст, который
	// влез в relay, но не влезет у гостя, отказывается здесь, до
	// подтверждения, а не становится unconfirmed после отправки.
	if _, err := ccmsg.EncodeExchange(guestFrameToken, guestFrameAddress, stepMessageSender(a.SwarmID), text, guestFrameID, "", 0); err != nil {
		return errors.New("the node instruction does not fit the guest session message")
	}
	return nil
}

// withPreviousStep ставит блок предыдущего шага между задачей прохода и
// текстом узла. Материал усекается, пока блок не уложится в
// maxPreviousStepBytes, а инструкция — в кадры relay и гостя: общий для
// частей материала предел ищется двоичным поиском, поэтому короткие части
// (вопрос, ответ оператора) остаются целыми, а длинная усекается первой.
// Заголовок блока и выбор оператора — текст графа и не усекаются; если не
// влезают и они, шаг отказывается, как с текстом узла сверх предела.
func withPreviousStep(a store.ActorStepAttempt, head, tail string) (string, error) {
	block := previousStepBlock(*a.Previous)
	compose := func(limit int) (string, bool) {
		b := block.render(limit)
		text := head + b + tail
		return text, len(b) <= maxPreviousStepBytes && deliverable(a, text) == nil
	}
	hi := min(block.longest(), maxPreviousStepBytes)
	if text, ok := compose(hi); ok && hi == block.longest() {
		return text, nil
	}
	text, ok := compose(0)
	if !ok {
		return "", errors.New("the node instruction leaves no room for the previous step")
	}
	// lo влезает, hi — нет.
	for lo := 0; lo+1 < hi; {
		mid := lo + (hi-lo)/2
		if t, ok := compose(mid); ok {
			lo, text = mid, t
		} else {
			hi = mid
		}
	}
	return text, nil
}

// stepBlock — блок предыдущего шага: заголовок и части по порядку.
type stepBlock struct {
	header string
	parts  []blockPart
}

// blockPart — часть блока: заголовок и текст. Материал шага (cut) усекается
// при нехватке места, текст графа — нет.
type blockPart struct {
	title, text string
	cut         bool
}

// previousStepBlock раскладывает предыдущий шаг (#248): после human-узла —
// вопрос, материал запроса, выбранный вариант и ответ оператора, после
// actor-узла — отчёт. Пустые части опускаются, кроме отчёта: его отсутствие
// названо, чтобы исполнитель не искал его выше. Текст обрезается по краям
// от пробелов, закрывающий тег конверта в нём заменяется пометкой.
func previousStepBlock(p store.PreviousStep) stepBlock {
	b := stepBlock{header: fmt.Sprintf("Previous step %d (node %s, outcome %s):\n", p.Sequence, withoutEnvelopeTag(p.NodeID), withoutEnvelopeTag(p.Outcome))}
	material := func(title, text string) {
		if text = strings.TrimSpace(text); text != "" {
			b.parts = append(b.parts, blockPart{title: title, text: withoutEnvelopeTag(text), cut: true})
		}
	}
	if r := p.Request; r != nil {
		material("Operator was asked:\n", r.Question)
		material("Material the operator saw:\n", r.Context)
		choice := p.Outcome
		if label, ok := r.OptionLabel(); ok {
			choice = label + " (" + p.Outcome + ")"
		}
		b.parts = append(b.parts, blockPart{title: "Operator chose: ", text: withoutEnvelopeTag(choice)})
		if r.Response != nil {
			material("Operator comment:\n", *r.Response)
		}
		return b
	}
	if material("Report:\n", p.Report); len(b.parts) == 0 {
		b.parts = append(b.parts, blockPart{text: "The actor left no report."})
	}
	return b
}

// render собирает блок, усекая каждую часть материала до limit байт.
func (b stepBlock) render(limit int) string {
	var s strings.Builder
	s.WriteString(b.header)
	for i, p := range b.parts {
		if i > 0 {
			s.WriteString("\n\n")
		}
		s.WriteString(p.title)
		if p.cut {
			s.WriteString(truncated(p.text, limit))
		} else {
			s.WriteString(p.text)
		}
	}
	s.WriteString("\n\n")
	return s.String()
}

// longest — длина самой длинной части материала: предел, при котором блок
// не усекается.
func (b stepBlock) longest() int {
	n := 0
	for _, p := range b.parts {
		if p.cut {
			n = max(n, len(p.text))
		}
	}
	return n
}

// truncated оставляет начало текста не длиннее limit байт по границе руны и
// называет, сколько байт опущено.
func truncated(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	mark := fmt.Sprintf("(truncated, %d bytes omitted)", len(text)-cut)
	if cut == 0 {
		return mark
	}
	return text[:cut] + "\n" + mark
}

func withoutEnvelopeTag(text string) string {
	return strings.ReplaceAll(text, envelopeClosingTag, envelopeTagRemoved)
}

// outcomeEffect описывает, куда ведёт исход актора: тот же выбор ребра, что
// применит граф (pipeline.Advance), без побочных эффектов.
func outcomeEffect(d *pipeline.Document, node, outcome string) (string, error) {
	t, err := pipeline.Advance(d, pipeline.Event{Node: node, State: "completed", Outcome: outcome})
	if err != nil {
		return "", err
	}
	switch {
	case t.Terminal:
		return "the swarm stops as failed", nil
	case t.Next == d.Exit:
		return "the swarm finishes as " + t.Execution, nil
	default:
		return "the swarm continues to node " + t.Next, nil
	}
}

// stepMessageSender — отправитель инструкции: рой, а не актор и не оператор.
func stepMessageSender(swarmID string) string { return "swarm:" + swarmID }
