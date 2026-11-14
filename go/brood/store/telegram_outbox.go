package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Исходящие уведомления Telegram (#279, #282, эпик #278): схема и
// ограждения — telegram_schema.go. Зеркало (#282): транзакция доменного
// изменения ставит уведомление каждому получателю (queueTelegram), а
// отправитель сервера (server/telegram_sender.go) после коммита берёт
// очередь пользователя по порядку. Строка несёт событие — рой и объект, а не
// текст: текст собирает слой сервера, хранилище молчит.

// TelegramEvent is the data of a Telegram notification: the swarm of the
// event and its object. The server renders the message from it and from the
// objects it names; the store keeps no text for the chat. Text that does not
// change once written (the question, material and answer of a request, the
// goal of a pass, the reason of a failed step) is read from its object when
// the message is rendered, not copied into the row of every recipient.
type TelegramEvent struct {
	// Swarm is the swarm of the event; every kind names one.
	Swarm string `json:"swarm"`
	// Request is the operator request of request_opened, actor_question,
	// request_answered and answer_delivery.
	Request string `json:"request,omitempty"`
	// Sequence is the step of the event: the step of a node request, the first
	// step of the pass of pass_started and pass_finished (the step that keeps
	// the pass goal), the failed step of step_failed.
	Sequence int64 `json:"sequence,omitempty"`
	// Node is the graph node of the step: the node of a node request, the
	// first node of a started pass, the node whose outcome finished a pass,
	// the node of a failed step.
	Node string `json:"node,omitempty"`
	// Execution is the swarm execution the event left: running or waiting
	// after pass_started; succeeded, failed or stopped after pass_finished.
	Execution string `json:"execution,omitempty"`
	// Delivery is the new delivery state of an answer to an actor
	// (answer_delivery): delivered, failed or unconfirmed.
	Delivery string `json:"delivery,omitempty"`
	// State is the state of a failed step (step_failed): refused or
	// unconfirmed. Its reason is the Detail of the step.
	State string `json:"state,omitempty"`
}

// telegramLiveLink: у пользователя u живая привязка l — пользователь
// включён, привязка linked и её сессия telegram существует, как у
// TelegramSessionActive.
const telegramLiveLink = `u.disabled = 0 AND l.state = 'linked'
	AND EXISTS (SELECT 1 FROM sessions se WHERE se.token_hash = l.session_hash AND se.kind = 'telegram' AND se.user_id = u.id)`

// telegramSwarmReader: пользователь u вправе читать рой w — то же правило,
// что у просмотра роя в app: рой не удалён, и пользователь — администратор
// либо участник проекта с ролью в рое. Отключённого пользователя отсекает
// telegramLiveLink.
const telegramSwarmReader = `w.state <> 'deleted' AND (u.is_admin = 1
	OR (EXISTS (SELECT 1 FROM project_members pm WHERE pm.project_id = w.project_id AND pm.user_id = u.id)
		AND EXISTS (SELECT 1 FROM swarm_members m WHERE m.swarm_id = w.id AND m.user_id = u.id)))`

// telegramSwarmManager: пользователь u вправе управлять роем w — то же
// правило, что у ответа на запрос (requireSwarmManager) и у управления роем в
// app: рой активен, и пользователь — администратор либо участник проекта с
// ролью owner или editor в рое. Отключённого пользователя отсекает
// telegramLiveLink. Только такому получателю уведомление предлагает кнопки
// ответа (#283); право ответа перепроверяет транзакция ответа.
const telegramSwarmManager = `w.state = 'active' AND (u.is_admin = 1
	OR (EXISTS (SELECT 1 FROM project_members pm WHERE pm.project_id = w.project_id AND pm.user_id = u.id)
		AND EXISTS (SELECT 1 FROM swarm_members m WHERE m.swarm_id = w.id AND m.user_id = u.id AND m.role IN ('owner', 'editor'))))`

// telegramSubscribed: привязка l подписана на вид, который даёт выражение
// SQL kind.
func telegramSubscribed(kind string) string {
	return `EXISTS (SELECT 1 FROM json_each(l.events) e WHERE e.value = ` + kind + `)`
}

// telegramRef — ссылка уведомления на объект события: запрос, проход или
// шаг роя.
func telegramRef(kind string, ev TelegramEvent) string {
	base := "swarm/" + ev.Swarm
	switch kind {
	case "pass_started", "pass_finished":
		return base + "/pass/" + strconv.FormatInt(ev.Sequence, 10)
	case "step_failed":
		return base + "/step/" + strconv.FormatInt(ev.Sequence, 10)
	}
	return base + "/request/" + ev.Request
}

// queueTelegram ставит уведомление вида kind о событии ev в транзакции
// доменного изменения, которая уже открыта в ctx: откат изменения уносит и
// уведомление. Получатель — каждый, кто в этот момент читает рой события, с
// живой привязкой и подпиской на вид; остальным строки нет. Одно событие
// вставляет строки по id пользователя, и у каждого пользователя они идут в
// порядке событий.
func (s *Store) queueTelegram(ctx context.Context, kind string, ev TelegramEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	now := nowRFC3339()
	_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, created_at, updated_at)
		SELECT u.id, ?, ?, ?, 'pending', 0, 0, ?, ?
		FROM users u JOIN user_telegram_links l ON l.user_id = u.id JOIN swarms w ON w.id = ?
		WHERE `+telegramLiveLink+` AND `+telegramSubscribed("?")+` AND `+telegramSwarmReader+`
		ORDER BY u.id`,
		kind, telegramRef(kind, ev), string(payload), now, now, ev.Swarm, kind)
	return err
}

var (
	// ErrTelegramUndeliverable: the head of the user's queue cannot be sent:
	// it has no recipient any more (no live link, no subscription to its
	// kind, no right to read its swarm, no bot to send it) or its event does
	// not decode. It was retired, settled failed without a send, and the next
	// notification heads the queue.
	ErrTelegramUndeliverable = errors.New("telegram notification cannot be delivered")
	// ErrTelegramInFlight: the head of the user's queue has an attempt in
	// flight; a second attempt could send its message twice.
	ErrTelegramInFlight = errors.New("telegram notification attempt is in flight")
	// ErrTelegramNoAttempt: the notification has no attempt from the queue in
	// flight to settle.
	ErrTelegramNoAttempt = errors.New("telegram notification has no attempt in flight")
	// ErrTelegramOutcomeInvalid refuses an outcome other than sent with its
	// message, failed (queued again or not) and unconfirmed.
	ErrTelegramOutcomeInvalid = errors.New("invalid telegram attempt outcome")
)

// TelegramNotification is a queued notification whose attempt the sender has
// begun: whom to send it to and what it is about.
type TelegramNotification struct {
	ID     int64
	UserID string
	Kind   string
	Ref    string
	Event  TelegramEvent
	// Attempts counts the attempts begun, this one included.
	Attempts  int64
	CreatedAt string
	// ChatID is the user's linked private chat.
	ChatID int64
	// OwnBot tells that the chat is linked through the user's own bot;
	// otherwise through the installation bot.
	OwnBot bool
	// Manager tells that the recipient may manage the swarm of the event at
	// the attempt: only such a recipient is offered answer buttons (#283).
	// The answer itself checks the right again in its transaction.
	Manager bool
}

// TelegramOutcome is what the sender learned of an attempt.
type TelegramOutcome struct {
	// State is sent, failed or unconfirmed.
	State string
	// MessageID is the Telegram message of a sent notification, required
	// with sent and only with it.
	MessageID int64
	// Requeue puts a failed notification back at the head of its queue for
	// the next attempt: the message certainly was not sent.
	Requeue bool
}

// telegramHead читает голову очереди пользователя и то, кому её можно
// отправить: живая привязка, подписка на вид, право читать рой события, и
// вправе ли получатель управлять роем (кнопки ответа, #283).
// json_extract падает на строке не JSON, и тогда голову не прочла бы ни одна
// попытка: рой события ищется только в валидном JSON, CASE вычисляется лениво.
const telegramHead = `SELECT o.id, o.kind, o.ref, o.payload, o.attempts, o.settled_attempts, o.created_at,
	coalesce(l.chat_id, 0), coalesce(l.bot_token IS NOT NULL, 0),
	CASE WHEN ` + telegramLiveLink + ` THEN 1 ELSE 0 END,
	CASE WHEN l.user_id IS NOT NULL AND EXISTS (SELECT 1 FROM json_each(l.events) e WHERE e.value = o.kind) THEN 1 ELSE 0 END,
	CASE WHEN ` + telegramSwarmReader + ` THEN 1 ELSE 0 END,
	CASE WHEN ` + telegramSwarmManager + ` THEN 1 ELSE 0 END
	FROM telegram_outbox o JOIN users u ON u.id = o.user_id
	LEFT JOIN user_telegram_links l ON l.user_id = o.user_id
	LEFT JOIN swarms w ON w.id = CASE WHEN json_valid(o.payload) THEN json_extract(o.payload, '$.swarm') END
	WHERE o.user_id = ? AND o.state = 'pending' ORDER BY o.id LIMIT 1`

// BeginTelegramAttempt records the attempt of the head of the user's queue,
// the pending notification with the lowest id, before the sender sends it,
// and returns it with the chat to send it to. installationBot tells whether
// the server has the installation bot, the only bot of a chat linked without
// an own bot. An empty queue is ErrNotFound; a head with an attempt in flight
// is ErrTelegramInFlight and gets no second attempt. A head nobody may
// receive any more, or whose event does not decode, is retired in the same
// transaction — a synthetic attempt settled failed, without a send, so
// retention can delete it — and the answer is ErrTelegramUndeliverable with
// the reason; the sender goes on with the next head.
func (s *Store) BeginTelegramAttempt(ctx context.Context, userID string, installationBot bool) (TelegramNotification, error) {
	var n TelegramNotification
	// retired — отказ снятой головы: снятие фиксируется, и только потом
	// вызывающий узнаёт об отказе.
	var retired error
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var payload string
		var settled int64
		var live, subscribed, reader bool
		err := s.conn(ctx).QueryRowContext(ctx, telegramHead, userID).Scan(&n.ID, &n.Kind, &n.Ref, &payload, &n.Attempts,
			&settled, &n.CreatedAt, &n.ChatID, &n.OwnBot, &live, &subscribed, &reader, &n.Manager)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: telegram queue of user %q is empty", ErrNotFound, userID)
		}
		if err != nil {
			return err
		}
		if n.Attempts > settled {
			return ErrTelegramInFlight
		}
		n.UserID = userID
		var reason string
		switch {
		case json.Unmarshal([]byte(payload), &n.Event) != nil:
			// Событие не прочесть (строка мимо хранилища или другой версии):
			// голову не отправить, а оставленная держала бы очередь вечно —
			// ограждения не дают ни удалить её, ни исправить.
			reason = "the notification payload is unreadable"
		case !live:
			reason = "the user has no live telegram link"
		case !subscribed:
			reason = "the user is not subscribed to " + n.Kind
		case !reader:
			reason = "the user may not read the swarm"
		case !n.OwnBot && !installationBot:
			reason = "the chat is linked through the installation bot, and there is none"
		}
		if err := s.beginTelegramAttempt(ctx, n.ID); err != nil {
			return err
		}
		n.Attempts++
		if reason == "" {
			return nil
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE telegram_outbox SET state = 'failed', settled_attempts = attempts, updated_at = ? WHERE id = ?`,
			nowRFC3339(), n.ID); err != nil {
			return err
		}
		retired = fmt.Errorf("%w: notification %d: %s", ErrTelegramUndeliverable, n.ID, reason)
		return nil
	})
	if err == nil {
		err = retired
	}
	if err != nil {
		return TelegramNotification{}, err
	}
	return n, nil
}

// beginTelegramAttempt отмечает попытку головы очереди до отправки. Условие
// записи повторяет проверку выборки: строка в очереди и без попытки в
// полёте.
func (s *Store) beginTelegramAttempt(ctx context.Context, id int64) error {
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE telegram_outbox SET attempts = attempts + 1, updated_at = ? WHERE id = ? AND state = 'pending' AND attempts = settled_attempts`,
		nowRFC3339(), id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrTelegramInFlight
	}
	return nil
}

// SettleTelegramAttempt records the outcome of the attempt in flight of a
// queued notification: sent with its Telegram message, failed, or
// unconfirmed. A failed one with Requeue goes back to the queue in the same
// transaction and heads it again: its message certainly was not sent. An
// outcome outside these is ErrTelegramOutcomeInvalid; a notification without an
// attempt from the queue in flight is ErrTelegramNoAttempt. Nothing is
// written on refusal.
func (s *Store) SettleTelegramAttempt(ctx context.Context, id int64, outcome TelegramOutcome) error {
	switch {
	case outcome.State != "sent" && outcome.State != "failed" && outcome.State != "unconfirmed",
		(outcome.State == "sent") != (outcome.MessageID > 0),
		outcome.Requeue && outcome.State != "failed":
		return fmt.Errorf("%w: an attempt settles sent with its message, failed (queued again or not) or unconfirmed", ErrTelegramOutcomeInvalid)
	}
	var message any
	if outcome.State == "sent" {
		message = outcome.MessageID
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		now := nowRFC3339()
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE telegram_outbox SET state = ?, settled_attempts = attempts, telegram_message_id = ?, updated_at = ? WHERE id = ? AND state = 'pending' AND attempts = settled_attempts + 1`,
			outcome.State, message, now, id)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return fmt.Errorf("%w: notification %d", ErrTelegramNoAttempt, id)
		}
		if !outcome.Requeue {
			return nil
		}
		// Исход failed записан, и уведомление снова в очереди: схема не даёт
		// закрыть попытку исходом pending одной записью.
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE telegram_outbox SET state = 'pending', updated_at = ? WHERE id = ?`, now, id)
		return err
	})
}

// PruneTelegramOutbox deletes the notifications whose outcome is recorded
// and was last written before cutoff: sent, failed and unconfirmed. A queued
// notification and an attempt in flight stay. It returns how many it
// deleted.
func (s *Store) PruneTelegramOutbox(ctx context.Context, cutoff time.Time) (int64, error) {
	// Моменты — RFC 3339 с переменной дробной частью: строкой они не
	// сравниваются, julianday сравнивает моменты.
	result, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM telegram_outbox WHERE state <> 'pending' AND attempts = settled_attempts AND julianday(updated_at) < julianday(?)`,
		cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// TelegramOutboxUsers returns the users with a Telegram queue: a pending
// notification, an attempt in flight included, in user id order.
func (s *Store) TelegramOutboxUsers(ctx context.Context) ([]string, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT DISTINCT user_id FROM telegram_outbox WHERE state = 'pending' ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []string{}
	for rows.Next() {
		var user string
		if err := rows.Scan(&user); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// RecoverTelegramOutbox settles every interrupted Telegram attempt as
// unconfirmed: its sendMessage may have reached the chat, so the
// notification is neither sent again from the queue nor marked failed, and
// only an explicit retry attempts it again. The sender calls it at startup,
// before its first attempt, after the previous server has stopped.
func (s *Store) RecoverTelegramOutbox(ctx context.Context) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE telegram_outbox SET state = 'unconfirmed', settled_attempts = attempts, updated_at = ? WHERE attempts > settled_attempts`,
		nowRFC3339())
	return err
}
