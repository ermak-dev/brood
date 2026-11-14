package server

// FEATURE: One linked Telegram chat both answers requests and runs commands
// (#283, #284, epic #278).
// SPEC: the poller hands a press of an answer or status button, a reply to a
// request message, plain text and /answer to the answers layer, and every
// other command and a press of a confirmation button to the commands
// adapter. /answer is the answer in any letter case, like the commands, and
// /help names both the commands and the ways to answer a request.
// IMPLEMENTATION: telegramPoller.handle (telegram_bots.go),
// telegram_answers.go, telegram_commands.go.
// STATUS: DONE

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLinkedChatAnswersRequestsAndRunsCommands(t *testing.T) {
	a := newAnswersHarness(t)
	node := a.openNode()
	// insertSwarmGraph пишет updated_at = 'now', а списки роёв, которые
	// читают команды, разбирают его временем.
	a.sql(`UPDATE swarms SET updated_at = ? WHERE id = 's1'`, time.Now().UTC().Format(time.RFC3339Nano))
	nodeMessage := a.sentRequest(a.alice, node)
	text := a.ask("text")
	mine := a.sentRequest(a.alice, text)

	if got := a.say(ownerChat, "/swarms"); !strings.HasPrefix(got, "Your swarms, page 1 of 1:") {
		t.Fatalf("/swarms = %q", got)
	}
	help := a.say(ownerChat, "/help")
	for _, want := range []string{"/swarms [page]", "/answer <number> <text>", "buttons of its message", "reply to its message"} {
		if !strings.Contains(help, want) {
			t.Errorf("/help does not name %q:\n%s", want, help)
		}
	}
	expectText(t, "plain text", a.say(ownerChat, "hello"), telegramReplyTextUsage)

	// Кнопка подтверждения — командам: отмена снимает её клавиатуру и не
	// трогает кнопки запроса.
	confirm := a.confirmationFor(ownerChat, "/stop release-train")
	cancel := confirm.Markup.InlineKeyboard[0][1].CallbackData
	expectText(t, "cancel press", a.press(ownerChat, confirm.ID, cancel), telegramPressCancel)
	if edits := a.editsOf(ownerChat, confirm.ID); len(edits) != 1 || edits[0].Markup != nil {
		t.Fatalf("edits of the confirmation: %+v", edits)
	}
	if edits := a.editsOf(ownerChat, nodeMessage.message); len(edits) != 0 {
		t.Fatalf("a confirmation press edited the request message: %+v", edits)
	}

	// /answer в любом регистре — ответ, а не неизвестная команда.
	reply := a.say(ownerChat, "/ANSWER "+strconv.FormatInt(mine.id, 10)+" use sqlite")
	answered := a.answeredBy(text.ID, a.alice, "", "use sqlite")
	expectText(t, "/ANSWER", reply, telegramRequestState(answered, "alice"))

	// Кнопка ответа — ответам.
	if got := a.press(ownerChat, nodeMessage.message, telegramAnswerData(nodeMessage.id, 0)); !strings.HasPrefix(got, "Answered by alice at ") {
		t.Fatalf("press of the answer button = %q", got)
	}
	a.answeredBy(node.ID, a.alice, "success", "")
}

// confirmationFor отправляет команду из чата chat и возвращает сообщение
// бота с кнопками подтверждения.
func (a *answersHarness) confirmationFor(chat int64, command string) fakeSent {
	a.t.Helper()
	before := len(a.api.sent(installBotToken))
	a.deliver(installBotToken, privateMessage(chat, "", command))
	for _, s := range a.api.sent(installBotToken)[before:] {
		if s.ChatID == chat && s.Markup != nil && strings.HasPrefix(s.Markup.InlineKeyboard[0][0].CallbackData, telegramConfirmPrefix) {
			return s
		}
	}
	a.t.Fatalf("%q got no confirmation", command)
	return fakeSent{}
}
