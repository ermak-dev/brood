package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Store reads of answers from Telegram (#283, epic #278).
// SPEC: an attempt tells whether its recipient may manage the swarm of the
// event at that moment (an enabled administrator, or a project member with
// the owner or editor role in the active swarm), so only such a recipient is
// offered answer buttons; a viewer, a reader of an inactive swarm and a
// notification of a vanished swarm are not. A button or /answer names a
// notification by its id, and the store finds it only among the notifications
// of the user who sent it; a reply names the message of a sent request
// notification in the chat of the user's current link. The messages an
// answer edits are the sent notifications that opened the request, of every
// user whose live link was made before the notification was queued, each
// with whether its user may read the swarm now: a message sent through an
// earlier link may lie in another chat or bot, where its number names another
// message.
// IMPLEMENTATION: BeginTelegramAttempt (telegram_outbox.go),
// UserTelegramNotification, TelegramRequestOfMessage and
// TelegramRequestMessages (telegram_answers.go).
// STATUS: DONE

// answersStore — mirrorStore с редактором e (чат 18) и запущенным approve:
// у читателей с привязкой в очереди начало прохода и запрос узла.
func answersStore(t *testing.T) (*Store, SwarmRequest) {
	t.Helper()
	ctx := context.Background()
	s := mirrorStore(t)
	seedUser(t, s, "e", "editor")
	mustNil(t, s.AddMember(ctx, "one", "e"))
	mustNil(t, s.SetSwarmMember(ctx, "one", "e", "editor"))
	linkTelegram(t, s, "e", "", 18)
	mustStart(t, s, "approve")
	return s, openRequestOf(t, s, "node")
}

// attemptOf начинает попытку уведомления вида kind пользователя user:
// уведомления до него в очереди закрываются failed.
func attemptOf(t *testing.T, s *Store, user, kind string) TelegramNotification {
	t.Helper()
	for {
		n := begin(t, s, user, true)
		if n.Kind == kind {
			return n
		}
		settle(t, s, n.ID, TelegramOutcome{State: "failed"})
	}
}

func TestAttemptTellsWhetherTheRecipientMayManageTheSwarm(t *testing.T) {
	s, _ := answersStore(t)
	for user, want := range map[string]bool{"u": true, "e": true, "root": true, "v": false} {
		n := attemptOf(t, s, user, "request_opened")
		if n.Manager != want {
			t.Errorf("attempt of %s: Manager = %t, want %t", user, n.Manager, want)
		}
		settle(t, s, n.ID, TelegramOutcome{State: "failed"})
	}
	// Рой, которым управлять нельзя, кнопок не даёт и owner: читать его ещё
	// можно, ответить — нет.
	_, err := s.db.Exec(`UPDATE swarms SET state = 'error' WHERE id = 'one'`)
	mustNil(t, err)
	_, err = s.db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, created_at, updated_at)
		VALUES ('u', 'request_answered', 'swarm/one/request/r', '{"swarm":"one","request":"r"}', 'pending', 0, 0, '2026-11-13T12:00:00Z', '2026-11-13T12:00:00Z')`)
	mustNil(t, err)
	if n := attemptOf(t, s, "u", "request_answered"); n.Manager {
		t.Errorf("attempt of the owner of an erred swarm: Manager = true")
	}
}

func TestUserTelegramNotificationIsOnlyTheUsersOwn(t *testing.T) {
	ctx := context.Background()
	s, req := answersStore(t)
	n := attemptOf(t, s, "u", "request_opened")
	got, err := s.UserTelegramNotification(ctx, "u", n.ID)
	mustNil(t, err)
	want := TelegramMessage{ID: n.ID, UserID: "u", Kind: "request_opened", Event: TelegramEvent{Swarm: "one", Request: req.ID, Sequence: 1, Node: "ask"}, State: "pending"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notification in flight = %+v, want %+v", got, want)
	}
	settle(t, s, n.ID, TelegramOutcome{State: "sent", MessageID: 77})
	got, err = s.UserTelegramNotification(ctx, "u", n.ID)
	mustNil(t, err)
	if got.State != "sent" || got.MessageID != 77 {
		t.Fatalf("sent notification = %+v, want sent as message 77", got)
	}
	// Номер чужого уведомления и несуществующий номер не находятся.
	for _, tc := range []struct {
		user string
		id   int64
	}{{"v", n.ID}, {"u", n.ID + 1000}, {"nobody", n.ID}} {
		if got, err := s.UserTelegramNotification(ctx, tc.user, tc.id); !errors.Is(err, ErrNotFound) {
			t.Errorf("notification %d of %s = %+v, %v; want not found", tc.id, tc.user, got, err)
		}
	}
}

// Ответ на сообщение (reply) называет его номером в чате: номер находит
// отправленное уведомление пользователя, открывшее запрос, только в чате его
// нынешней привязки.
func TestReplyFindsTheRequestNotificationOfItsMessageInTheCurrentLink(t *testing.T) {
	ctx := context.Background()
	s, req := answersStore(t)
	n := attemptOf(t, s, "u", "request_opened")
	// Попытка в полёте: номера сообщения ещё нет.
	if got, err := s.TelegramRequestOfMessage(ctx, "u", 77); !errors.Is(err, ErrNotFound) {
		t.Fatalf("message of a notification in flight = %+v, %v; want not found", got, err)
	}
	settle(t, s, n.ID, TelegramOutcome{State: "sent", MessageID: 77})
	got, err := s.TelegramRequestOfMessage(ctx, "u", 77)
	mustNil(t, err)
	want := TelegramMessage{ID: n.ID, UserID: "u", Kind: "request_opened", Event: TelegramEvent{Swarm: "one", Request: req.ID, Sequence: 1, Node: "ask"}, State: "sent", MessageID: 77}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message 77 of u = %+v, want %+v", got, want)
	}
	// Сообщение другого пользователя и уведомление, которое запроса не
	// открывает, ответа не принимают.
	settle(t, s, attemptOf(t, s, "v", "request_opened").ID, TelegramOutcome{State: "sent", MessageID: 78})
	_, err = s.db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, created_at, updated_at)
		VALUES ('u', 'request_answered', ?, ?, 'pending', 0, 0, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`,
		"swarm/one/request/"+req.ID, `{"swarm":"one","request":"`+req.ID+`"}`)
	mustNil(t, err)
	settle(t, s, attemptOf(t, s, "u", "request_answered").ID, TelegramOutcome{State: "sent", MessageID: 79})
	for _, message := range []int64{78, 79, 80} {
		if got, err := s.TelegramRequestOfMessage(ctx, "u", message); !errors.Is(err, ErrNotFound) {
			t.Errorf("message %d of u = %+v, %v; want not found", message, got, err)
		}
	}
	// Привязка заново — новая сессия: номер 77 прежней привязки в новом чате
	// называет чужое сообщение.
	_, err = s.UnlinkTelegram(ctx, "u")
	mustNil(t, err)
	linkTelegram(t, s, "u", "", 11)
	if got, err := s.TelegramRequestOfMessage(ctx, "u", 77); !errors.Is(err, ErrNotFound) {
		t.Fatalf("message 77 after u linked again = %+v, %v; want not found", got, err)
	}
}

func TestRequestMessagesAreTheSentOnesInTheCurrentLinkWithTheReadRight(t *testing.T) {
	ctx := context.Background()
	s, req := answersStore(t)
	sent := map[string]int64{}
	for i, user := range []string{"root", "u", "v", "e"} {
		n := attemptOf(t, s, user, "request_opened")
		if user == "e" {
			// Попытка в полёте: номера сообщения ещё нет.
			continue
		}
		sent[user] = n.ID
		settle(t, s, n.ID, TelegramOutcome{State: "sent", MessageID: int64(100 + i)})
	}
	event := TelegramEvent{Swarm: "one", Request: req.ID, Sequence: 1, Node: "ask"}
	message := func(user string, number, chat int64, reader bool) TelegramMessage {
		return TelegramMessage{ID: sent[user], UserID: user, Kind: "request_opened", Event: event, State: "sent", MessageID: number, ChatID: chat, Reader: reader}
	}
	messages, err := s.TelegramRequestMessages(ctx, "one", req.ID)
	mustNil(t, err)
	// Viewer тоже читает рой: его сообщение покажет состояние запроса.
	if want := []TelegramMessage{message("root", 100, 13, true), message("u", 101, 11, true), message("v", 102, 12, true)}; !reflect.DeepEqual(messages, want) {
		t.Fatalf("request messages = %+v\nwant %+v", messages, want)
	}
	// Кто больше не читает рой, остаётся в списке: кнопки его сообщения
	// снимаются, но состояния запроса он не видит.
	mustNil(t, s.RemoveSwarmMember(ctx, "one", "v"))
	messages, err = s.TelegramRequestMessages(ctx, "one", req.ID)
	mustNil(t, err)
	if want := []TelegramMessage{message("root", 100, 13, true), message("u", 101, 11, true), message("v", 102, 12, false)}; !reflect.DeepEqual(messages, want) {
		t.Fatalf("request messages after v left the swarm = %+v\nwant %+v", messages, want)
	}
	// Привязка заново — новая сессия: сообщение прежней привязки могло уйти в
	// другой чат или через другой бот, и его номер там чужой.
	_, err = s.UnlinkTelegram(ctx, "root")
	mustNil(t, err)
	linkTelegram(t, s, "root", "", 13)
	messages, err = s.TelegramRequestMessages(ctx, "one", req.ID)
	mustNil(t, err)
	if len(messages) != 2 || messages[0].UserID != "u" || messages[1].UserID != "v" {
		t.Fatalf("request messages after root linked again = %+v, want u and v", messages)
	}
	// Другой запрос своих сообщений не делит.
	if messages, err := s.TelegramRequestMessages(ctx, "one", "another"); err != nil || len(messages) != 0 {
		t.Fatalf("messages of another request = %+v, %v", messages, err)
	}
}
