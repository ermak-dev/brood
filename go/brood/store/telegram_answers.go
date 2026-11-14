package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Ответы из Telegram (#283, эпик #278): кнопка и /answer называют
// уведомление его номером в outbox, reply — номером его сообщения в чате, а
// правка сообщений после ответа находит отправленные уведомления запроса.
// Здесь только чтение: ответ пишет AnswerSwarmRequest с правами и сессией в
// своей транзакции.

// TelegramMessage is a notification as the chat refers to it: a button or
// /answer names it by ID, a reply by its message, and an answer edits the
// message it was sent as.
type TelegramMessage struct {
	ID     int64
	UserID string
	Kind   string
	Event  TelegramEvent
	State  string
	// MessageID is the Telegram message of a sent notification, zero before.
	MessageID int64
	// ChatID and OwnBot are the chat of the user's live link and whether it is
	// linked through the user's own bot; Reader tells that the user may read
	// the swarm of the request now. Only TelegramRequestMessages sets them.
	ChatID int64
	OwnBot bool
	Reader bool
}

// telegramRequestKinds — виды уведомлений, которые открывают запрос и несут
// его кнопки.
const telegramRequestKinds = `('request_opened', 'actor_question')`

// telegramCurrentLinkMessage — отправленное уведомление o, открывшее запрос,
// у пользователя u с живой привязкой l и её сессией se, поставленное после
// того, как эта привязка сделана. Постановка берёт живую привязку, поэтому
// такое уведомление ушло через бот и в чат этой привязки; ушедшее через
// прежнюю привязку могло попасть в другой чат или бот, где его номер
// сообщения называет чужое сообщение. Моменты — RFC 3339 с переменной
// дробной частью: julianday сравнивает моменты, строка — нет.
const telegramCurrentLinkMessage = `telegram_outbox o JOIN users u ON u.id = o.user_id
	JOIN user_telegram_links l ON l.user_id = o.user_id
	JOIN sessions se ON se.token_hash = l.session_hash
	WHERE o.kind IN ` + telegramRequestKinds + ` AND o.state = 'sent' AND ` + telegramLiveLink + `
		AND julianday(o.created_at) >= julianday(se.created_at)`

// UserTelegramNotification returns the notification id of the user. A
// notification of another user and an unknown id are ErrNotFound: a chat
// reaches only its own notifications.
func (s *Store) UserTelegramNotification(ctx context.Context, userID string, id int64) (TelegramMessage, error) {
	m := TelegramMessage{ID: id, UserID: userID}
	var payload string
	var message sql.NullInt64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT kind, payload, state, telegram_message_id FROM telegram_outbox WHERE id = ? AND user_id = ?`,
		id, userID).Scan(&m.Kind, &payload, &m.State, &message)
	if errors.Is(err, sql.ErrNoRows) {
		return TelegramMessage{}, fmt.Errorf("%w: telegram notification %d of user %q", ErrNotFound, id, userID)
	}
	if err != nil {
		return TelegramMessage{}, err
	}
	if err := json.Unmarshal([]byte(payload), &m.Event); err != nil {
		return TelegramMessage{}, fmt.Errorf("decode telegram notification %d: %w", id, err)
	}
	m.MessageID = message.Int64
	return m, nil
}

// TelegramRequestOfMessage returns the sent notification of the user that
// opened a request (request_opened, actor_question) and that Telegram shows as
// message messageID in the chat of the user's current link: a reply to that
// message answers its request. A notification sent through an earlier link
// does not count, since its number names a message of another chat or bot.
// Nothing found: ErrNotFound.
func (s *Store) TelegramRequestOfMessage(ctx context.Context, userID string, messageID int64) (TelegramMessage, error) {
	m := TelegramMessage{UserID: userID}
	var payload string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT o.id, o.kind, o.payload, o.state, o.telegram_message_id
		FROM `+telegramCurrentLinkMessage+` AND o.user_id = ? AND o.telegram_message_id = ?
		ORDER BY o.id DESC LIMIT 1`, userID, messageID).Scan(&m.ID, &m.Kind, &payload, &m.State, &m.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return TelegramMessage{}, fmt.Errorf("%w: telegram message %d of user %q", ErrNotFound, messageID, userID)
	}
	if err != nil {
		return TelegramMessage{}, err
	}
	if err := json.Unmarshal([]byte(payload), &m.Event); err != nil {
		return TelegramMessage{}, fmt.Errorf("decode telegram notification %d: %w", m.ID, err)
	}
	return m, nil
}

// TelegramRequestMessages returns the sent notifications that opened the
// request (request_opened, actor_question), with their chats, in id order: of
// every user whose current link was made before the notification was queued,
// and with whether the user may read the swarm now. Rights at the edit do not
// pick the messages: a manager demoted or removed after the send still holds
// answer buttons, and a viewer's message may show the state of the request as
// well.
func (s *Store) TelegramRequestMessages(ctx context.Context, swarmID, requestID string) ([]TelegramMessage, error) {
	ref := telegramRef("request_opened", TelegramEvent{Swarm: swarmID, Request: requestID})
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT o.id, o.user_id, o.kind, o.payload, o.state, o.telegram_message_id, l.chat_id, l.bot_token IS NOT NULL,
		coalesce((SELECT 1 FROM swarms w WHERE w.id = ? AND `+telegramSwarmReader+`), 0)
		FROM `+telegramCurrentLinkMessage+` AND o.ref = ?
		ORDER BY o.id`, swarmID, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TelegramMessage{}
	for rows.Next() {
		var m TelegramMessage
		var payload string
		if err := rows.Scan(&m.ID, &m.UserID, &m.Kind, &payload, &m.State, &m.MessageID, &m.ChatID, &m.OwnBot, &m.Reader); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &m.Event); err != nil {
			return nil, fmt.Errorf("decode telegram notification %d: %w", m.ID, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
