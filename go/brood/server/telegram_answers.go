package server

// Ответы на запросы оператору из Telegram (#283, эпик #278). Нажатие кнопки,
// reply на сообщение текстового запроса и /answer <номер> <текст>
// привязанного чата записываются тем же методом хранилища, что
// POST /api/swarms/{id}/requests/{request}/answer (answerRequest), от имени
// пользователя привязки: сессия telegram чата проверяется в транзакции
// ответа вместо cookie панели, права — там же. Повтор нажатия, который шлёт
// Telegram, попадает под правило хранилища «тот же автор с тем же исходом и
// текстом — успех». Отказ — короткий текст с причиной: ответ на нажатие
// (answerCallbackQuery) или сообщение в чат; закрытый запрос называет своё
// состояние. Доставка ответа актору идёт своей горутиной, одновременно с
// ответом чату, а не после него: она длится до requestDeliveryTimeout, а
// поллер бота установки один на всех его пользователей.

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

// Ответы чату на нажатие, reply и /answer. Причина говорит, что случилось, а
// не откуда: текст ошибок хранилища в чат не уходит. Действие ответ называет,
// только если оно есть: отменить запрос ушедшего актора brood не даёт.
const (
	telegramReplyAnswerUsage  = "Send /answer <number> <text>: the number is in the request message."
	telegramReplyTextUsage    = "To answer a text request, reply to its message with the answer or send /answer <number> <text>."
	telegramReplyButtonStale  = "This button is no longer valid."
	telegramReplyRequestGone  = "This request no longer exists."
	telegramReplyDenied       = "You may not answer this request: that takes the owner or editor role in the active swarm."
	telegramReplyStale        = "The asking actor is gone: this request can no longer be answered."
	telegramReplyInvalid      = "This answer does not fit the request."
	telegramReplyNotNow       = "This request cannot be answered right now."
	telegramReplyAnswerFailed = "brood could not record the answer right now. Try again later."
	telegramReplyHidden       = "You may no longer see this request."
	// telegramCallbackTextLimit — предел текста answerCallbackQuery Bot API.
	telegramCallbackTextLimit = 200
)

// telegramReplyNoRequest — /answer называет уведомление, которого у чата нет.
func telegramReplyNoRequest(notification int64) string {
	return "There is no request " + strconv.FormatInt(notification, 10) + " in this chat."
}

// telegramReplyNotText — /answer или reply называет запрос с кнопками.
func telegramReplyNotText(notification int64) string {
	return "Request " + strconv.FormatInt(notification, 10) + " is answered with its buttons, not with text."
}

// telegramAnswers — то, что поллеру нужно от сервера для ответа: путь ответа
// панели, доставка ответа актору, правка сообщений закрытого запроса и право
// читать рой.
type telegramAnswers interface {
	answerRequest(ctx context.Context, answer store.SwarmAnswer, authorize func(context.Context) bool) (store.SwarmRequest, error)
	deliverAnswer(ctx context.Context, userID string, req store.SwarmRequest)
	requestClosed(swarmID, requestID string)
	canViewSwarm(userID, swarmID string) bool
}

// deliverAnswer доставляет записанный ответ спросившему актору от имени
// пользователя userID, как после ответа из панели; исход пишет хранилище, и
// его зеркалит answer_delivery (#282).
func (s *Server) deliverAnswer(ctx context.Context, userID string, req store.SwarmRequest) {
	s.deliverSwarmAnswer(ctx, store.User{ID: userID}, req, false)
}

// requestClosed правит сообщения Telegram закрытого запроса.
func (s *Server) requestClosed(swarmID, requestID string) {
	s.sender.requestClosed(swarmID, requestID)
}

// canViewSwarm: пользователь сейчас вправе читать рой — правило просмотра
// роя прикладного слоя; сбой проверки — нет.
func (s *Server) canViewSwarm(userID, swarmID string) bool {
	allowed, err := s.app.Can(userID, app.ActionViewSwarm, app.SwarmRes(swarmID))
	return err == nil && allowed
}

// handleCallback разбирает нажатие кнопки сообщения бота, кроме кнопки
// подтверждения команды (c:): её поллер отдаёт telegramCommands (#284).
// Только личный чат человека, привязанный через этот бот; нажатие из иного
// чата игнорируется без ответа, как его сообщения. Ответ на нажатие —
// состояние запроса после него или причина отказа.
func (p *telegramPoller) handleCallback(ctx context.Context, q *telegram.CallbackQuery) {
	m := q.Message
	if q.From.IsBot || q.From.ID <= 0 || m == nil || !m.Chat.Private() || m.Chat.ID != q.From.ID {
		return
	}
	chat, err := p.chatOwner(ctx, m.Chat.ID, q.From.ID)
	if err != nil || !chat.linked() {
		return
	}
	p.noteSender(ctx, chat, &q.From)
	text := p.press(ctx, chat.userID, m.Chat.ID, q.From.ID, q.Data)
	_ = p.client.AnswerCallbackQuery(ctx, q.ID, telegramClipText(text, telegramCallbackTextLimit))
}

// press — исход нажатия кнопки data: кнопка состояния показывает
// состояние запроса тому, кто ещё читает рой, кнопка варианта отвечает им
// на запрос.
func (p *telegramPoller) press(ctx context.Context, owner string, chat, from int64, data string) string {
	button, ok := parseTelegramButton(data)
	if !ok {
		return telegramReplyButtonStale
	}
	req, err := p.requestOf(ctx, owner, button.notification)
	switch {
	case errors.Is(err, errTelegramNoRequest):
		return telegramReplyButtonStale
	case err != nil:
		return telegramRequestRefusal(err)
	case button.status && (p.bots.answers == nil || !p.bots.answers.canViewSwarm(owner, req.SwarmID)):
		// Уведомление пришло, когда пользователь читал рой; состояние
		// запроса после этого ему не принадлежит.
		return telegramReplyHidden
	case button.status:
		return p.stateOf(ctx, req)
	}
	outcome, response, ok := telegramButtonAnswer(req, button.option)
	if !ok {
		return telegramReplyButtonStale
	}
	return p.answer(ctx, owner, chat, from, store.SwarmAnswer{SwarmID: req.SwarmID, RequestID: req.ID, UserID: owner, Outcome: outcome, Response: response})
}

// answerCommand — исход /answer <номер> <текст>: текстовый ответ на запрос
// уведомления с этим номером в этом чате.
func (p *telegramPoller) answerCommand(ctx context.Context, owner string, m *telegram.Message) string {
	id, text, ok := telegramAnswerArgs(m.Text)
	if !ok {
		return telegramReplyAnswerUsage
	}
	return p.answerText(ctx, owner, m, id, text)
}

// textReply — исход текста без команды. Reply на сообщение запроса — ответ на
// этот запрос, как /answer с номером его уведомления: Telegram сам открывает
// reply на сообщение, и номер искать не нужно. Прочий текст, и reply на
// сообщение, которое запроса этого чата не открывало, получают подсказку,
// как ответить.
func (p *telegramPoller) textReply(ctx context.Context, owner string, m *telegram.Message) string {
	text := strings.TrimSpace(m.Text)
	if m.ReplyToMessage == nil || text == "" {
		return telegramReplyTextUsage
	}
	n, err := p.bots.store.TelegramRequestOfMessage(ctx, owner, m.ReplyToMessage.MessageID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return telegramReplyTextUsage
	case err != nil:
		return telegramReplyAnswerFailed
	}
	return p.answerText(ctx, owner, m, n.ID, text)
}

// answerText — текстовый ответ text из сообщения m на запрос уведомления с
// номером id: общий путь /answer и reply.
func (p *telegramPoller) answerText(ctx context.Context, owner string, m *telegram.Message, id int64, text string) string {
	req, err := p.requestOf(ctx, owner, id)
	switch {
	case errors.Is(err, errTelegramNoRequest):
		return telegramReplyNoRequest(id)
	case err != nil:
		return telegramRequestRefusal(err)
	case req.Kind != "text":
		return telegramReplyNotText(id)
	}
	return p.answer(ctx, owner, m.Chat.ID, m.From.ID, store.SwarmAnswer{SwarmID: req.SwarmID, RequestID: req.ID, UserID: owner, Response: text})
}

// errTelegramNoRequest: у пользователя нет уведомления с этим номером,
// открывшего запрос.
var errTelegramNoRequest = errors.New("telegram notification opens no request of the user")

// requestOf находит запрос уведомления owner с номером id. Чужое
// уведомление и уведомление, которое запроса не открывает, —
// errTelegramNoRequest: чат видит только свои запросы.
func (p *telegramPoller) requestOf(ctx context.Context, owner string, id int64) (store.SwarmRequest, error) {
	n, err := p.bots.store.UserTelegramNotification(ctx, owner, id)
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && !telegramOpensRequest(n.Kind):
		return store.SwarmRequest{}, errTelegramNoRequest
	case err != nil:
		return store.SwarmRequest{}, err
	}
	return p.bots.store.GetSwarmRequest(ctx, n.Event.Swarm, n.Event.Request)
}

// answer записывает ответ путём сервера от имени owner. Сессию заменяет
// привязка чата chat с собеседником from через этот бот, проверенная в
// транзакции ответа. Закрытый запрос отвечает своим состоянием, и его
// сообщения правятся; ответ, ждущий доставки, уходит актору своей горутиной,
// пока чату отвечает вызывающий.
func (p *telegramPoller) answer(ctx context.Context, owner string, chat, from int64, answer store.SwarmAnswer) string {
	if p.bots.answers == nil {
		return telegramReplyAnswerFailed
	}
	req, err := p.bots.answers.answerRequest(ctx, answer, func(ctx context.Context) bool {
		active, err := p.bots.store.TelegramSessionActive(ctx, owner, p.botUsername, chat, from)
		return err == nil && active
	})
	if errors.Is(err, store.ErrPipelineVisitConflict) {
		current, err := p.bots.store.GetSwarmRequest(ctx, answer.SwarmID, answer.RequestID)
		if err != nil {
			return telegramRequestRefusal(err)
		}
		if current.State == "open" {
			// Узловой запрос, чей граф больше не ждёт ответа.
			return telegramReplyNotNow
		}
		// Сообщения закрытого запроса могли остаться с кнопками.
		p.bots.answers.requestClosed(current.SwarmID, current.ID)
		return p.stateOf(ctx, current)
	}
	if err != nil {
		return telegramRequestRefusal(err)
	}
	if req.Delivery == "pending" {
		p.bots.answering.Add(1)
		go func() {
			defer p.bots.answering.Done()
			p.bots.answers.deliverAnswer(ctx, owner, req)
		}()
	}
	return p.stateOf(ctx, req)
}

// stateOf — состояние запроса для чата с именем автора ответа.
func (p *telegramPoller) stateOf(ctx context.Context, req store.SwarmRequest) string {
	return telegramRequestState(req, telegramUserName(ctx, p.bots.store, req.AnsweredBy))
}

// telegramRequestRefusal — причина отказа ответа для чата.
func telegramRequestRefusal(err error) string {
	switch {
	case errors.Is(err, store.ErrSwarmRequestDenied):
		return telegramReplyDenied
	case errors.Is(err, store.ErrSwarmRequestStale):
		return telegramReplyStale
	case errors.Is(err, store.ErrNotFound):
		return telegramReplyRequestGone
	case errors.Is(err, store.ErrSwarmRequestInvalid):
		return telegramReplyInvalid
	}
	return telegramReplyAnswerFailed
}
