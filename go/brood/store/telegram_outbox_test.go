package store

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// FEATURE: Telegram outbox fences (#279, epic #278).
// SPEC: a notification is inserted queued: pending, without attempts or a
// message; it settles as sent, failed or unconfirmed. sendMessage is not
// idempotent, so the schema keeps what the sender learned: an attempt is
// recorded before the send (attempts grows) and settled after it
// (settled_attempts catches up), so an interrupted attempt is told apart from
// a queued retry. An unsettled attempt is never attempted again or settled
// back into the queue; startup recovery settles it as unconfirmed. Only a
// sent notification may carry the reply field flag (force_reply, #298). A
// sent notification is never rewritten, its reply field included, an
// unconfirmed one (it may have reached the chat) never becomes failed or goes
// back to the queue, attempts never
// decrease, the notification itself (recipient, kind, reference, payload)
// never changes, through UPDATE or REPLACE in either recursive_triggers mode.
// A queued notification and an attempt in flight are never deleted while
// their user exists; a settled one may be deleted by retention, and its id is
// never given to another notification, neither by AUTOINCREMENT nor by an
// explicit id at or below the AUTOINCREMENT counter. The kind CHECK is exactly
// the event vocabulary. Deleting a user finds its notifications by an index.
// A failed notification may be queued again explicitly; an unconfirmed one
// may be attempted again explicitly, outside the queue.
// IMPLEMENTATION: telegram_outbox CHECKs and AUTOINCREMENT id, the index
// telegram_outbox_user, the triggers telegram_outbox_insert,
// telegram_outbox_fence, telegram_outbox_attempts,
// telegram_outbox_unconfirmed, telegram_outbox_no_replace,
// telegram_outbox_no_delete and telegram_outbox_fresh_id
// (telegram_schema.go), RecoverTelegramOutbox (telegram_outbox.go).
// STATUS: DONE

// outboxStore: пользователи u1 и u2 и одно уведомление u1 в очереди.
func outboxStore(t *testing.T) (*Store, int64) {
	t.Helper()
	s := telegramStore(t)
	return s, queueNotification(t, s, "u1", "request_opened")
}

func queueNotification(t *testing.T, s *Store, user, kind string) int64 {
	t.Helper()
	result, err := s.db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, created_at, updated_at)
		VALUES (?, ?, 'swarm/s/request/r', '{"text":"Ship?"}', 'pending', 0, 0, '2026-11-13T12:00:00Z', '2026-11-13T12:00:00Z')`, user, kind)
	mustNil(t, err)
	id, err := result.LastInsertId()
	mustNil(t, err)
	return id
}

// outboxRow: строка уведомления SQL-литералами, как tableRows.
func outboxRow(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var row string
	mustNil(t, s.db.QueryRow(`SELECT quote(id)||','||quote(user_id)||','||quote(kind)||','||quote(ref)||','||quote(payload)||','||
		quote(state)||','||quote(attempts)||','||quote(settled_attempts)||','||quote(telegram_message_id)||','||quote(created_at)||','||quote(updated_at)||','||
		quote(force_reply)
		FROM telegram_outbox WHERE id = ?`, id).Scan(&row))
	return row
}

// outboxState: состояние и попытки уведомления как «state:attempts/settled».
func outboxState(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var state string
	mustNil(t, s.db.QueryRow(`SELECT state || ':' || attempts || '/' || settled_attempts FROM telegram_outbox WHERE id = ?`, id).Scan(&state))
	return state
}

func outboxExec(s *Store, id int64, set string) error {
	_, err := s.db.Exec(fmt.Sprintf(`UPDATE telegram_outbox SET %s WHERE id = %d`, set, id))
	return err
}

// outboxSteps применяет переходы отправителя по порядку.
func outboxSteps(t *testing.T, s *Store, id int64, steps ...string) {
	t.Helper()
	for _, step := range steps {
		if err := outboxExec(s, id, step); err != nil {
			t.Fatalf("sender transition %q refused: %v", step, err)
		}
	}
}

// refusedBy: каждая запись отвергнута ограждением с текстом fence, строка
// уведомления не изменилась.
func refusedBy(t *testing.T, s *Store, id int64, fence string, sets ...string) {
	t.Helper()
	before := outboxRow(t, s, id)
	for _, set := range sets {
		if err := outboxExec(s, id, set); err == nil || !strings.Contains(err.Error(), fence) {
			t.Fatalf("%q from %s: %v, want %q", set, outboxState(t, s, id), err, fence)
		}
	}
	if after := outboxRow(t, s, id); after != before {
		t.Fatalf("notification changed:\n got %s\nwant %s", after, before)
	}
}

const attemptFence = "telegram notification attempt is out of order"

func TestOutboxSettlesThroughTheSenderTransitions(t *testing.T) {
	s, id := outboxStore(t)
	outboxSteps(t, s, id,
		// Попытка отмечается до отправки: обрыв на ней — attempts больше
		// settled_attempts.
		`attempts = 1, updated_at = 'a1'`,
		`state = 'failed', settled_attempts = 1, updated_at = 'f1'`,
		// Явный повтор: снова в очередь со следующей попыткой.
		`state = 'pending', attempts = 2, updated_at = 'a2'`,
		`state = 'unconfirmed', settled_attempts = 2, updated_at = 'u2'`,
		// Повтор неизвестного исхода идёт без очереди и остаётся unconfirmed.
		`attempts = 3, updated_at = 'a3'`,
		`state = 'sent', settled_attempts = 3, telegram_message_id = 1001, updated_at = 's3'`,
	)
	if got := outboxState(t, s, id); got != "sent:3/3" {
		t.Fatalf("settled notification = %s", got)
	}
	// Попытку из очереди закрывает любой из трёх исходов.
	for _, outcome := range []string{`state = 'sent', telegram_message_id = 7`, `state = 'failed'`, `state = 'unconfirmed'`} {
		other := queueNotification(t, s, "u1", "pass_started")
		outboxSteps(t, s, other, `attempts = 1`, outcome+`, settled_attempts = 1`)
	}
	// Явно повторённый failed стоит в очереди с записанным исходом прежней
	// попытки, и отправитель начинает следующую.
	queued := queueNotification(t, s, "u1", "pass_finished")
	outboxSteps(t, s, queued, `attempts = 1`, `state = 'failed', settled_attempts = 1`, `state = 'pending'`)
	if got := outboxState(t, s, queued); got != "pending:1/1" {
		t.Fatalf("retried notification = %s, want queued with its attempt settled", got)
	}
	outboxSteps(t, s, queued, `attempts = 2`, `state = 'sent', settled_attempts = 2, telegram_message_id = 8`)
}

// TestInterruptedAttemptIsNeverAttemptedAgain: попытка без записанного исхода
// могла дойти до чата. Ни следующая попытка, ни возврат в очередь её не
// перекрывают; исход записывает только сама попытка.
func TestInterruptedAttemptIsNeverAttemptedAgain(t *testing.T) {
	s, id := outboxStore(t)
	outboxSteps(t, s, id, `attempts = 1`)
	refusedBy(t, s, id, attemptFence,
		`attempts = 2`,
		// Записать исход прерванной попытки и тут же начать следующую.
		`attempts = 2, settled_attempts = 1`,
		// Исход «в очередь» стёр бы попытку, которая могла дойти.
		`settled_attempts = 1`,
		// Исход пишется вместе с закрытием попытки.
		`state = 'unconfirmed'`,
	)
	// Повтор failed из очереди — тоже попытка: прерванная, она не повторяется.
	retried := queueNotification(t, s, "u1", "pass_started")
	outboxSteps(t, s, retried, `attempts = 1`, `state = 'failed', settled_attempts = 1`, `state = 'pending'`, `attempts = 2`)
	refusedBy(t, s, retried, attemptFence, `attempts = 3`, `attempts = 3, settled_attempts = 2`, `settled_attempts = 2`)
	// Явный повтор unconfirmed — попытка вне очереди с теми же правилами.
	unknown := queueNotification(t, s, "u1", "pass_finished")
	outboxSteps(t, s, unknown, `attempts = 1`, `state = 'unconfirmed', settled_attempts = 1`, `attempts = 2`)
	refusedBy(t, s, unknown, attemptFence, `attempts = 3`, `attempts = 3, settled_attempts = 2`)
}

// TestAttemptIsRecordedBeforeItsOutcome: исход без отмеченной попытки
// означал бы sendMessage, который обрыв не оставил бы в базе.
func TestAttemptIsRecordedBeforeItsOutcome(t *testing.T) {
	s, id := outboxStore(t)
	refusedBy(t, s, id, attemptFence,
		`attempts = 1, settled_attempts = 1, state = 'failed'`,
		`attempts = 1, settled_attempts = 1, state = 'sent', telegram_message_id = 7`,
		`attempts = 1, settled_attempts = 1, state = 'unconfirmed'`,
	)
	// Повторённый failed в очереди не становится исходом без новой попытки.
	outboxSteps(t, s, id, `attempts = 1`, `state = 'failed', settled_attempts = 1`)
	refusedBy(t, s, id, attemptFence, `state = 'unconfirmed'`)
	outboxSteps(t, s, id, `state = 'pending'`)
	refusedBy(t, s, id, attemptFence, `state = 'sent', telegram_message_id = 7`, `state = 'failed'`, `state = 'unconfirmed'`)
	// Неизвестный исход не становится отправленным без новой попытки: номер
	// сообщения записывает только попытка, которая его получила.
	unknown := queueNotification(t, s, "u1", "pass_started")
	outboxSteps(t, s, unknown, `attempts = 1`, `state = 'unconfirmed', settled_attempts = 1`)
	refusedBy(t, s, unknown, attemptFence, `state = 'sent', telegram_message_id = 7`)
}

// TestRecoveryMarksInterruptedAttemptsUnconfirmed: восстановление при запуске
// записывает исход прерванных попыток — unconfirmed, без повтора; очередь,
// повторённые failed и записанные исходы не трогает.
func TestRecoveryMarksInterruptedAttemptsUnconfirmed(t *testing.T) {
	ctx := context.Background()
	s, interrupted := outboxStore(t)
	outboxSteps(t, s, interrupted, `attempts = 1`)
	queued := queueNotification(t, s, "u1", "pass_started")
	retried := queueNotification(t, s, "u1", "pass_finished")
	outboxSteps(t, s, retried, `attempts = 1`, `state = 'failed', settled_attempts = 1`, `state = 'pending'`)
	failed := queueNotification(t, s, "u2", "step_failed")
	outboxSteps(t, s, failed, `attempts = 1`, `state = 'failed', settled_attempts = 1`)
	sent := queueNotification(t, s, "u2", "actor_question")
	outboxSteps(t, s, sent, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 9`)
	explicit := queueNotification(t, s, "u2", "answer_delivery")
	outboxSteps(t, s, explicit, `attempts = 1`, `state = 'unconfirmed', settled_attempts = 1`, `attempts = 2`)
	retriedInFlight := queueNotification(t, s, "u2", "request_answered")
	outboxSteps(t, s, retriedInFlight, `attempts = 1`, `state = 'failed', settled_attempts = 1`, `state = 'pending', attempts = 2`)

	untouched := map[int64]string{}
	for _, id := range []int64{queued, retried, failed, sent} {
		untouched[id] = outboxRow(t, s, id)
	}
	for range 2 {
		mustNil(t, s.RecoverTelegramOutbox(ctx))
	}
	for id, want := range map[int64]string{interrupted: "unconfirmed:1/1", explicit: "unconfirmed:2/2", retriedInFlight: "unconfirmed:2/2"} {
		if got := outboxState(t, s, id); got != want {
			t.Fatalf("recovered notification %d = %s, want %s", id, got, want)
		}
	}
	for id, want := range untouched {
		if got := outboxRow(t, s, id); got != want {
			t.Fatalf("recovery changed a settled or queued notification:\n got %s\nwant %s", got, want)
		}
	}
	// Восстановленная попытка — неизвестный исход: не failed, не в очередь,
	// повтор только явный.
	refusedBy(t, s, interrupted, "unconfirmed telegram notification may have been sent", `state = 'failed'`, `state = 'pending'`)
	outboxSteps(t, s, interrupted, `attempts = 2`)
}

func TestSentNotificationIsNeverRewritten(t *testing.T) {
	s, id := outboxStore(t)
	outboxSteps(t, s, id, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 1001, updated_at = 'sent'`)
	refusedBy(t, s, id, "telegram notification is immutable",
		`state = 'pending', telegram_message_id = NULL`,
		`state = 'failed', telegram_message_id = NULL`,
		`state = 'unconfirmed', telegram_message_id = NULL`,
		`telegram_message_id = 1002`,
		`attempts = 2`,
		`settled_attempts = 0`,
		`updated_at = 'later'`,
		`payload = '{"text":"other"}'`,
		// Поле ответа (#298) — свойство ушедшего сообщения: его не приписать
		// и не снять.
		`force_reply = 1`,
	)
	// Запись без изменений не переписывает и проходит.
	before := outboxRow(t, s, id)
	mustNil(t, outboxExec(s, id, `state = state`))
	if after := outboxRow(t, s, id); after != before {
		t.Fatalf("sent notification changed:\n got %s\nwant %s", after, before)
	}
	field := queueNotification(t, s, "u1", "actor_question")
	outboxSteps(t, s, field, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 1003, force_reply = 1`)
	refusedBy(t, s, field, "telegram notification is immutable", `force_reply = 0`)
}

func TestUnconfirmedNotificationNeverBecomesFailed(t *testing.T) {
	const fence = "unconfirmed telegram notification may have been sent"
	s, id := outboxStore(t)
	outboxSteps(t, s, id, `attempts = 1`, `state = 'unconfirmed', settled_attempts = 1`)
	refusedBy(t, s, id, fence,
		`state = 'failed'`,
		`state = 'failed', attempts = 2`,
		// Очередь допускает failed следующей попытки: возврат в pending — тоже отказ.
		`state = 'pending', attempts = 2`,
		`state = 'pending'`,
	)
	// Явный повтор не делает исход известным: и его попытка не пишет failed.
	outboxSteps(t, s, id, `attempts = 2`)
	refusedBy(t, s, id, fence, `state = 'failed', settled_attempts = 2`, `state = 'pending', settled_attempts = 2`)
}

func TestNotificationIdentityAndAttemptsAreFenced(t *testing.T) {
	s, id := outboxStore(t)
	outboxSteps(t, s, id, `attempts = 1`, `state = 'failed', settled_attempts = 1`, `state = 'pending', attempts = 2`)
	refusedBy(t, s, id, "telegram notification is immutable",
		`id = 999`,
		`user_id = 'u2'`,
		`kind = 'pass_started'`,
		`ref = 'swarm/s/request/other'`,
		`payload = '{"text":"other"}'`,
		`created_at = 'later'`,
		`attempts = 1`,
		`settled_attempts = 0`,
	)
}

// TestOutboxKeepsQueuedAndInFlightNotifications: уведомление в очереди и
// попытка в полёте не удаляются, пока жив пользователь: удаление стёрло бы
// то, что ещё должно уйти, или запись о попытке, которая могла дойти.
// Уведомление с записанным исходом (sent, failed, unconfirmed) удаляет срок
// хранения, иначе таблица росла бы без предела. Номер удалённого не
// достаётся новому уведомлению. С пользователем уходят все
// (TestDeletingAUserDeletesItsTelegramRows).
func TestOutboxKeepsQueuedAndInFlightNotifications(t *testing.T) {
	const fence = "telegram notification is queued or in flight"
	s, queued := outboxStore(t)
	retried := queueNotification(t, s, "u1", "pass_started")
	outboxSteps(t, s, retried, `attempts = 1`, `state = 'failed', settled_attempts = 1`, `state = 'pending'`)
	inFlight := queueNotification(t, s, "u1", "pass_finished")
	outboxSteps(t, s, inFlight, `attempts = 1`)
	explicit := queueNotification(t, s, "u1", "step_failed")
	outboxSteps(t, s, explicit, `attempts = 1`, `state = 'unconfirmed', settled_attempts = 1`, `attempts = 2`)
	kept := databaseRows(t, s.db)["telegram_outbox"]
	for _, id := range []int64{queued, retried, inFlight, explicit} {
		if _, err := s.db.Exec(`DELETE FROM telegram_outbox WHERE id = ?`, id); err == nil || !strings.Contains(err.Error(), fence) {
			t.Fatalf("notification %s deleted: %v", outboxState(t, s, id), err)
		}
	}
	if after := databaseRows(t, s.db)["telegram_outbox"]; !reflect.DeepEqual(after, kept) {
		t.Fatalf("outbox changed:\n got %v\nwant %v", after, kept)
	}
	var settled []int64
	for _, outcome := range []string{`state = 'sent', telegram_message_id = 1001`, `state = 'failed'`, `state = 'unconfirmed'`} {
		id := queueNotification(t, s, "u2", "actor_question")
		outboxSteps(t, s, id, `attempts = 1`, outcome+`, settled_attempts = 1`)
		settled = append(settled, id)
	}
	for _, id := range settled {
		if _, err := s.db.Exec(`DELETE FROM telegram_outbox WHERE id = ?`, id); err != nil {
			t.Fatalf("retention of settled notification %d refused: %v", id, err)
		}
	}
	if after := databaseRows(t, s.db)["telegram_outbox"]; !reflect.DeepEqual(after, kept) {
		t.Fatalf("retention changed the kept notifications:\n got %v\nwant %v", after, kept)
	}
	// Номер не переиспользуется: новое уведомление не выглядит удалённым.
	if next, last := queueNotification(t, s, "u2", "request_opened"), settled[len(settled)-1]; next <= last {
		t.Fatalf("new notification took id %d, notifications up to %d were deleted", next, last)
	}
}

// TestOutboxNeverGivesAnIssuedIdAgain: номер удалённого уведомления не
// достаётся другому и явной вставкой: AUTOINCREMENT держит только номера,
// которые выбирает сам. Отвергается любой явный номер не дальше счётчика
// sqlite_sequence, и номер последнего удалённого, и номер под ним, во всех
// видах INSERT. Номер дальше счётчика ещё ничей и проходит, а следующий
// номер AUTOINCREMENT идёт уже за ним.
func TestOutboxNeverGivesAnIssuedIdAgain(t *testing.T) {
	const fence = "telegram notification id is not new"
	s, _ := outboxStore(t)
	var retained []int64
	for range 2 {
		id := queueNotification(t, s, "u1", "pass_started")
		outboxSteps(t, s, id, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 1001`)
		retained = append(retained, id)
	}
	for _, id := range retained {
		_, err := s.db.Exec(`DELETE FROM telegram_outbox WHERE id = ?`, id)
		mustNil(t, err)
	}
	insert := func(verb string, id int64) error {
		_, err := s.db.Exec(verb+` INTO telegram_outbox (id, user_id, kind, ref, payload, state, attempts, settled_attempts, created_at, updated_at)
			VALUES (?, 'u2', 'request_opened', 'swarm/s/request/other', '{}', 'pending', 0, 0, 'now', 'now')`, id)
		return err
	}
	kept := databaseRows(t, s.db)["telegram_outbox"]
	for _, verb := range []string{"INSERT", "INSERT OR REPLACE", "INSERT OR IGNORE", "REPLACE"} {
		for _, id := range retained {
			if err := insert(verb, id); err == nil || !strings.Contains(err.Error(), fence) {
				t.Fatalf("%s of retained id %d: %v, want %q", verb, id, err, fence)
			}
		}
	}
	if after := databaseRows(t, s.db)["telegram_outbox"]; !reflect.DeepEqual(after, kept) {
		t.Fatalf("outbox changed:\n got %v\nwant %v", after, kept)
	}
	last := retained[len(retained)-1]
	next := queueNotification(t, s, "u2", "request_opened")
	if next <= last {
		t.Fatalf("new notification took id %d, notifications up to %d were deleted", next, last)
	}
	ahead := next + 10
	if err := insert("INSERT", ahead); err != nil {
		t.Fatalf("explicit id %d past the counter refused: %v", ahead, err)
	}
	if after := queueNotification(t, s, "u2", "request_opened"); after <= ahead {
		t.Fatalf("new notification took id %d after the explicit id %d", after, ahead)
	}
}

// TestNotificationIsQueuedPendingAtInsert: уведомление входит в очередь
// pending, без попыток и сообщения: попытку отмечает и закрывает только
// отправитель. Иначе вставка записала бы отправленным то, что не уходило,
// или попытку в полёте, которую восстановление сочло бы неизвестным исходом.
func TestNotificationIsQueuedPendingAtInsert(t *testing.T) {
	s, _ := outboxStore(t)
	before := databaseRows(t, s.db)["telegram_outbox"]
	for name, values := range map[string]string{
		"sent":              `'sent', 1, 1, 1001`,
		"unconfirmed":       `'unconfirmed', 1, 1, NULL`,
		"failed":            `'failed', 3, 3, NULL`,
		"attempt in flight": `'pending', 1, 0, NULL`,
		"queued retry":      `'pending', 1, 1, NULL`,
	} {
		_, err := s.db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, telegram_message_id, created_at, updated_at)
			VALUES ('u1', 'request_opened', 'r', '{}', ` + values + `, 'now', 'now')`)
		if err == nil || !strings.Contains(err.Error(), "telegram notification is queued pending") {
			t.Fatalf("%s inserted: %v, want the queued pending refusal", name, err)
		}
	}
	if after := databaseRows(t, s.db)["telegram_outbox"]; !reflect.DeepEqual(after, before) {
		t.Fatalf("outbox changed:\n got %v\nwant %v", after, before)
	}
}

// TestOutboxChecksKeepTheVocabularyAndTheSettledShape: CHECK — вторая линия
// за telegram_outbox_insert, который отвергает вставку не в очереди раньше
// них. Тест снимает триггер вставки и проверяет сами CHECK.
func TestOutboxChecksKeepTheVocabularyAndTheSettledShape(t *testing.T) {
	s, id := outboxStore(t)
	_, err := s.db.Exec(`DROP TRIGGER telegram_outbox_insert`)
	mustNil(t, err)
	for _, kind := range TelegramEventKinds() {
		queueNotification(t, s, "u2", kind)
	}
	insert := func(values string) error {
		_, err := s.db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, telegram_message_id, created_at, updated_at) VALUES ` + values)
		return err
	}
	for name, values := range map[string]string{
		"unknown kind":              `('u1', 'request_opend', 'r', '{}', 'pending', 0, 0, NULL, 'now', 'now')`,
		"unknown state":             `('u1', 'request_opened', 'r', '{}', 'sending', 0, 0, NULL, 'now', 'now')`,
		"empty reference":           `('u1', 'request_opened', '', '{}', 'pending', 0, 0, NULL, 'now', 'now')`,
		"sent without message":      `('u1', 'request_opened', 'r', '{}', 'sent', 1, 1, NULL, 'now', 'now')`,
		"message while queued":      `('u1', 'request_opened', 'r', '{}', 'pending', 1, 1, 5, 'now', 'now')`,
		"settled unattempted":       `('u1', 'request_opened', 'r', '{}', 'failed', 0, 0, NULL, 'now', 'now')`,
		"unconfirmed never settled": `('u1', 'request_opened', 'r', '{}', 'unconfirmed', 1, 0, NULL, 'now', 'now')`,
		"failed in flight":          `('u1', 'request_opened', 'r', '{}', 'failed', 2, 1, NULL, 'now', 'now')`,
		"sent in flight":            `('u1', 'request_opened', 'r', '{}', 'sent', 2, 1, 5, 'now', 'now')`,
		"two attempts in flight":    `('u1', 'request_opened', 'r', '{}', 'pending', 2, 0, NULL, 'now', 'now')`,
		"settled beyond attempts":   `('u1', 'request_opened', 'r', '{}', 'pending', 0, 1, NULL, 'now', 'now')`,
		"negative settled":          `('u1', 'request_opened', 'r', '{}', 'pending', 0, -1, NULL, 'now', 'now')`,
		"negative attempts":         `('u1', 'request_opened', 'r', '{}', 'pending', -1, -1, NULL, 'now', 'now')`,
	} {
		if err := insert(values); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Fatalf("%s: %v, want a CHECK refusal", name, err)
		}
	}
	if err := insert(`('ghost', 'request_opened', 'r', '{}', 'pending', 0, 0, NULL, 'now', 'now')`); !IsForeignKeyViolation(err) {
		t.Fatalf("unknown user: %v, want a foreign key violation", err)
	}
	outboxSteps(t, s, id, `attempts = 1`)
	if err := outboxExec(s, id, `state = 'sent', settled_attempts = 1`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("sent without a Telegram message id: %v", err)
	}
	// Поле ответа (#298) — флаг, и есть только у ушедшего сообщения.
	for name, set := range map[string]string{
		"reply field in flight":       `force_reply = 1`,
		"reply field of a failed":     `state = 'failed', settled_attempts = 1, force_reply = 1`,
		"reply field of unconfirmed":  `state = 'unconfirmed', settled_attempts = 1, force_reply = 1`,
		"reply field that is no flag": `state = 'sent', settled_attempts = 1, telegram_message_id = 5, force_reply = 2`,
	} {
		if err := outboxExec(s, id, set); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Fatalf("%s: %v, want a CHECK refusal", name, err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, created_at, updated_at, force_reply)
		VALUES ('u1', 'request_opened', 'r', '{}', 'pending', 0, 0, 'now', 'now', 1)`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("queued with the reply field: %v, want a CHECK refusal", err)
	}
}

// outboxKindCheck: список IN у CHECK колонки kind в тексте таблицы.
var outboxKindCheck = regexp.MustCompile(`\bkind\s+TEXT\b[^,]*\bCHECK\s*\(\s*kind\s+IN\s*\(([^)]*)\)`)

// TestOutboxKindCheckIsTheEventVocabulary: CHECK telegram_outbox.kind —
// ровно словарь TelegramEventKinds в его порядке. Лишний вид в CHECK пустил
// бы в очередь уведомление, на которое нельзя подписаться, а вид словаря вне
// CHECK — подписку, по которой не запишется ни одно уведомление.
// TestOutboxChecksKeepTheVocabularyAndTheSettledShape проверяет только
// второе направление.
func TestOutboxKindCheckIsTheEventVocabulary(t *testing.T) {
	s := newTestStore(t)
	var table string
	mustNil(t, s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'telegram_outbox'`).Scan(&table))
	match := outboxKindCheck.FindStringSubmatch(table)
	if match == nil {
		t.Fatalf("telegram_outbox has no CHECK of kind IN (...):\n%s", table)
	}
	var kinds []string
	for _, item := range strings.Split(match[1], ",") {
		kinds = append(kinds, strings.Trim(strings.TrimSpace(item), "'"))
	}
	if want := TelegramEventKinds(); !reflect.DeepEqual(kinds, want) {
		t.Fatalf("telegram_outbox.kind CHECK = %q, want the event vocabulary %q", kinds, want)
	}
}

func TestSentNotificationSurvivesReplaceInBothTriggerModes(t *testing.T) {
	for _, mode := range []string{"OFF", "ON"} {
		t.Run("recursive_"+mode, func(t *testing.T) {
			s, id := outboxStore(t)
			outboxSteps(t, s, id, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 1001`)
			setRecursive(t, s, mode)
			before := databaseRows(t, s.db)["telegram_outbox"]
			for _, statement := range []string{
				fmt.Sprintf(`INSERT OR REPLACE INTO telegram_outbox SELECT id, user_id, kind, ref, payload, 'pending', 0, 0, NULL, created_at, updated_at FROM telegram_outbox WHERE id = %d`, id),
				fmt.Sprintf(`REPLACE INTO telegram_outbox SELECT id, user_id, kind, ref, payload, state, attempts, settled_attempts, 1002, created_at, updated_at FROM telegram_outbox WHERE id = %d`, id),
				fmt.Sprintf(`INSERT INTO telegram_outbox SELECT id, user_id, kind, ref, payload, 'pending', 0, 0, NULL, created_at, updated_at FROM telegram_outbox WHERE id = %d`, id),
			} {
				if _, err := s.db.Exec(statement); err == nil {
					t.Fatalf("sent notification replaced through %s", statement)
				}
			}
			if after := databaseRows(t, s.db)["telegram_outbox"]; !reflect.DeepEqual(after, before) {
				t.Fatalf("outbox changed:\n got %v\nwant %v", after, before)
			}
		})
	}
}

// queryPlan: строки EXPLAIN QUERY PLAN запроса.
func queryPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	mustNil(t, err)
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		mustNil(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	mustNil(t, rows.Err())
	return strings.Join(plan, "\n")
}

// TestOutboxQueueIsReadInOrderPerUser: отправитель берёт очередь
// пользователя по порядку id частичным индексом, а не просмотром таблицы.
// Индекс частичный: строки с записанным исходом в него не входят, и запрос
// без условия очереди его не использует.
func TestOutboxQueueIsReadInOrderPerUser(t *testing.T) {
	s, first := outboxStore(t)
	second := queueNotification(t, s, "u1", "pass_started")
	if second <= first {
		t.Fatalf("queue ids %d then %d are not increasing", first, second)
	}
	queue := queryPlan(t, s, `SELECT id FROM telegram_outbox WHERE user_id = ? AND state = 'pending' ORDER BY id LIMIT 1`, "u1")
	if !strings.Contains(queue, "telegram_outbox_pending") || strings.Contains(queue, "TEMP B-TREE") {
		t.Fatalf("queue read does not use telegram_outbox_pending in id order:\n%s", queue)
	}
	if history := queryPlan(t, s, `SELECT id FROM telegram_outbox WHERE user_id = ? ORDER BY id`, "u1"); strings.Contains(history, "telegram_outbox_pending") {
		t.Fatalf("a read of every notification uses the queue index, so it is not partial:\n%s", history)
	}
}

// TestDeletingAUserFindsItsNotificationsByIndex: каскад удаления пользователя
// ищет его уведомления запросом по user_id, которому частичный индекс очереди
// не годится. Без своего индекса каждое удаление пользователя читало бы всю
// таблицу.
func TestDeletingAUserFindsItsNotificationsByIndex(t *testing.T) {
	s, _ := outboxStore(t)
	// Так SQLite ищет дочерние строки внешнего ключа при ON DELETE CASCADE.
	if plan := queryPlan(t, s, `SELECT rowid FROM telegram_outbox WHERE user_id = ?`, "u1"); !strings.Contains(plan, "telegram_outbox_user") {
		t.Fatalf("the cascade lookup of a user's notifications does not use telegram_outbox_user:\n%s", plan)
	}
}
