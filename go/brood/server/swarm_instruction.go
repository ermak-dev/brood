package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/proxmox/guest/ccmsg"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// maxStepInstructionBytes ограничивает текст инструкции шага. Экранирование
// JSON может раздуть его вшестеро, поэтому кадры relay и гостя проверяются
// отдельно (stepInstruction).
const maxStepInstructionBytes = 64 << 10

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

// stepInstruction собирает первое сообщение шага (#212): текст узла
// (Node.Prompt) и минимальный контекст — рой, узел, номер шага, исходы по
// рёбрам графа, как отчитаться (исход и отчёт о сделанном, #233) и как
// спросить оператора. Больше ничего:
// инструкция не выдаёт прав и не заменяет проверок, текст узла — не граница
// безопасности. Чистая функция попытки.
func stepInstruction(a store.ActorStepAttempt) (string, error) {
	if a.Graph == nil {
		return "", errors.New("the swarm graph is missing")
	}
	node, ok := a.Graph.Nodes[a.NodeID]
	if !ok || node.Type != "actor" {
		return "", fmt.Errorf("node %s is not an actor node", a.NodeID)
	}
	prompt := strings.TrimSpace(node.Prompt)
	if prompt == "" {
		prompt = "(the node has no instruction text)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[brood swarm %s] Step %d, node %s.\n\n%s\n\n", a.SwarmName, a.Sequence, a.NodeID, prompt)
	fmt.Fprintf(&b, "When the step is done, report its outcome with pipeline_visit_complete: run_id %s, sequence %d, request_id %s, outcome success or fail.\n", a.SwarmID, a.Sequence, a.RequestID)
	for _, outcome := range []string{"success", "fail"} {
		effect, err := outcomeEffect(a.Graph, a.NodeID, outcome)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "- %s: %s.\n", outcome, effect)
	}
	b.WriteString("Pass report to pipeline_visit_complete as well: what you did and where the result is (branch, merge request or document). The operator sees it with the outcome; it is kept once and cannot be changed.\n")
	b.WriteString("To ask the human operator a question, use ask_operator; the answer arrives as a message.\n")
	b.WriteString("This message is from the swarm workflow, not from an actor: do not reply to it with send.")
	text := b.String()
	if strings.Contains(text, envelopeClosingTag) {
		return "", errors.New("the node instruction contains the message envelope closing tag")
	}
	if len(text) > maxStepInstructionBytes {
		return "", fmt.Errorf("the node instruction exceeds %d bytes", maxStepInstructionBytes)
	}
	frame, err := json.Marshal(relay.MessageRequest{To: a.Actor.Name, From: stepMessageSender(a.SwarmID), Body: text})
	if err != nil || len(frame) > relay.MaxMessageBytes {
		return "", errors.New("the node instruction does not fit a relay message")
	}
	// Гость сверяет с пределом не кадр relay, а свой NDJSON: auth-кадр,
	// конверт и тело, заново экранированное в user-кадре. Текст, который
	// влез в relay, но не влезет у гостя, отказывается здесь, до
	// подтверждения, а не становится unconfirmed после отправки.
	if _, err := ccmsg.EncodeExchange(guestFrameToken, guestFrameAddress, stepMessageSender(a.SwarmID), text, guestFrameID, "", 0); err != nil {
		return "", errors.New("the node instruction does not fit the guest session message")
	}
	return text, nil
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
