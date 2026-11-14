package server

// FEATURE: Confirmations of Telegram commands (#284).
// SPEC: a confirmation belongs to one linked chat (user, bot, chat) under
// the telegram session of its link, and has a short random id, carried in
// callback data within the 64 bytes of the Bot API with the choice. A chat
// unlinked and linked again has another session: a confirmation of the
// earlier link is then merely invalid. A press of its chat runs it at most once: the
// first confirming press within the TTL moves it to running and hands over
// the action, every later press, the same callback again included, only
// learns that it runs or ran. A cancel or a press at or after the TTL ends
// it without the action, for good. An action pressed in time begins no
// later than a short grace after the TTL; past it the confirmation expires
// without the action. An unknown id, another chat and malformed
// data run nothing. A chat keeps a bounded number of open confirmations: a
// new one expires the oldest; settled ones are forgotten after a while, and
// a press then is merely invalid. The memory of a chat is bounded too: past
// a limit, a new confirmation forgets the oldest one that is not running.
// Data of another kind of button (#283) is refused without touching its
// keyboard.
// IMPLEMENTATION: telegramConfirmations (telegram_confirm.go).
// STATUS: DONE

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfirmations(clock *telegramClock) *telegramConfirmations {
	return newTelegramConfirmations(clock.Now)
}

var testChat = telegramChat{userID: "u1", botUsername: "", chatID: 42, session: "session-1"}

// action — действие подтверждения, которое считает свои запуски.
type countedAction struct {
	mu   sync.Mutex
	runs int
}

func (a *countedAction) run(context.Context) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runs++
	return "done"
}

func (a *countedAction) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs
}

func TestTelegramConfirmationRunsOncePerConfirmation(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := testConfirmations(clock)
	action := &countedAction{}
	id, err := c.add(testChat, action.run)
	if err != nil {
		t.Fatal(err)
	}
	for _, yes := range []bool{true, false} {
		data := telegramConfirmData(id, yes)
		if len(data) > 64 {
			t.Fatalf("callback data of %d bytes", len(data))
		}
		got, choice, ok := parseTelegramConfirm(data)
		if !ok || got != id || choice != yes {
			t.Fatalf("parse %q = %q %v %v", data, got, choice, ok)
		}
	}
	first := c.press(testChat, id, true)
	if first.run == nil || first.answer != "Running." || !first.clear {
		t.Fatalf("first press = %+v", first)
	}
	// Пока действие идёт, повтор его не запускает.
	if again := c.press(testChat, id, true); again.run != nil || again.answer != "This action is already running." {
		t.Fatalf("press while running = %+v", again)
	}
	first.run(context.Background())
	c.finish(id)
	for _, yes := range []bool{true, false} {
		if again := c.press(testChat, id, yes); again.run != nil || again.answer != "This action already ran; it is not repeated." {
			t.Fatalf("press after the run = %+v", again)
		}
	}
	if action.count() != 1 {
		t.Fatalf("the action ran %d times", action.count())
	}
}

func TestTelegramConfirmationPressesRaceToOneRun(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := testConfirmations(clock)
	id, err := c.add(testChat, (&countedAction{}).run)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	runs := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.press(testChat, id, true).run != nil {
				mu.Lock()
				runs++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if runs != 1 {
		t.Fatalf("%d concurrent presses got the action", runs)
	}
}

func TestTelegramConfirmationTimeoutAndCancelRunNothing(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := testConfirmations(clock)
	expiring, _ := c.add(testChat, (&countedAction{}).run)
	cancelled, _ := c.add(testChat, (&countedAction{}).run)
	if got := c.press(testChat, cancelled, false); got.run != nil || got.answer != "Cancelled; nothing was done." || !got.clear {
		t.Fatalf("cancel = %+v", got)
	}
	if got := c.press(testChat, cancelled, true); got.run != nil || got.answer != "This confirmation was cancelled; nothing was done." {
		t.Fatalf("confirm after cancel = %+v", got)
	}
	// Срок — первый момент, когда подтверждение уже не действует.
	clock.Advance(telegramConfirmTTL)
	for range 2 {
		if got := c.press(testChat, expiring, true); got.run != nil || got.answer != "This confirmation expired; nothing was done." || !got.clear {
			t.Fatalf("press at the TTL = %+v", got)
		}
	}
	// Истёкшее не оживает, даже если часы пошли назад.
	clock.Advance(-time.Minute)
	if got := c.press(testChat, expiring, true); got.run != nil {
		t.Fatalf("an expired confirmation ran after the clock went back: %+v", got)
	}
}

// Действие нажатия, принятого в срок, начинается не позже
// telegramConfirmGrace после срока: позже подтверждение истекает, ничего не
// исполнив, и повтор нажатия узнаёт, что оно истекло.
func TestTelegramConfirmationActionStartsWithinTheGrace(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := testConfirmations(clock)
	late, _ := c.add(testChat, (&countedAction{}).run)
	inTime, _ := c.add(testChat, (&countedAction{}).run)
	clock.Advance(telegramConfirmTTL - time.Second)
	for _, id := range []string{late, inTime} {
		if got := c.press(testChat, id, true); got.run == nil {
			t.Fatalf("a press before the TTL = %+v", got)
		}
	}
	clock.Advance(telegramConfirmGrace)
	if !c.begin(inTime) {
		t.Fatal("an action pressed in time did not begin within the grace")
	}
	c.finish(inTime)
	// Срок и отсрочка — первый момент, когда действие уже не начинается.
	clock.Advance(time.Second)
	if c.begin(late) {
		t.Fatal("an action began past the TTL and its grace")
	}
	c.finish(late)
	for range 2 {
		if got := c.press(testChat, late, true); got.run != nil || got.answer != "This confirmation expired; nothing was done." || !got.clear {
			t.Fatalf("press after a late start = %+v", got)
		}
	}
	if got := c.press(testChat, inTime, true); got.run != nil || got.answer != "This action already ran; it is not repeated." {
		t.Fatalf("press after an action within the grace = %+v", got)
	}
	if c.begin("unknown") {
		t.Fatal("an unknown confirmation began")
	}
}

func TestTelegramConfirmationBelongsToOneChat(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := testConfirmations(clock)
	id, _ := c.add(testChat, (&countedAction{}).run)
	for _, other := range []telegramChat{
		{userID: "u2", chatID: 42},
		{userID: "u1", chatID: 43},
		{userID: "u1", botUsername: "own_bot", chatID: 42},
		// Тот же чат под новой привязкой.
		{userID: "u1", chatID: 42, session: "session-2"},
	} {
		if got := c.press(other, id, true); got.run != nil || got.answer != "This button is no longer valid; nothing was done." {
			t.Fatalf("press from %+v = %+v", other, got)
		}
	}
	if got := c.press(testChat, "unknown", true); got.run != nil || got.answer != "This button is no longer valid; nothing was done." {
		t.Fatalf("unknown id = %+v", got)
	}
	// Чужие нажатия подтверждение не тронули.
	if got := c.press(testChat, id, true); got.run == nil {
		t.Fatalf("the owner's press after foreign ones = %+v", got)
	}
	for _, data := range []string{"", "c:", "c::y", "c:" + id, "c:" + id + ":", "c:" + id + ":yes", "x:" + id + ":y", "c:" + id + ":y:z"} {
		if _, _, ok := parseTelegramConfirm(data); ok {
			t.Fatalf("malformed data %q parsed", data)
		}
	}
}

func TestTelegramConfirmationIDsAreRandomAndShort(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := testConfirmations(clock)
	seen := map[string]bool{}
	for range 64 {
		id, err := c.add(testChat, (&countedAction{}).run)
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] || len(id) < 16 || strings.ContainsAny(id, ":") {
			t.Fatalf("confirmation id %q", id)
		}
		seen[id] = true
	}
	c.random = bytes.NewReader(nil)
	if id, err := c.add(testChat, (&countedAction{}).run); err == nil {
		t.Fatalf("a short random source gave the id %q", id)
	}
}

func TestTelegramConfirmationsAreBoundedPerChat(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := testConfirmations(clock)
	var ids []string
	for range telegramConfirmPerChat + 1 {
		id, err := c.add(testChat, (&countedAction{}).run)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		clock.Advance(time.Second)
	}
	// Самое старое открытое подтверждение истекло, остальные действуют.
	if got := c.press(testChat, ids[0], true); got.run != nil || got.answer != "This confirmation expired; nothing was done." {
		t.Fatalf("the oldest confirmation over the bound = %+v", got)
	}
	if got := c.press(testChat, ids[1], true); got.run == nil {
		t.Fatalf("a confirmation within the bound = %+v", got)
	}
	other := telegramChat{userID: "u2", chatID: 99, session: "session-9"}
	if _, err := c.add(other, (&countedAction{}).run); err != nil {
		t.Fatal(err)
	}
	if got := c.press(testChat, ids[2], true); got.run == nil {
		t.Fatalf("another chat's confirmation expired this chat's: %+v", got)
	}
	// Исход помнится срок хранения после TTL, затем забывается: нажатие лишь
	// недействительно.
	c.finish(ids[1])
	clock.Advance(telegramConfirmTTL + telegramConfirmKeep)
	if _, err := c.add(other, (&countedAction{}).run); err != nil {
		t.Fatal(err)
	}
	if got := c.press(testChat, ids[1], true); got.run != nil || got.answer != "This button is no longer valid; nothing was done." {
		t.Fatalf("a forgotten confirmation = %+v", got)
	}
	if n := c.size(); n != 1 {
		t.Fatalf("%d confirmations kept after the keep period, want the new one", n)
	}
}

func TestTelegramConfirmationMemoryIsBoundedPerChat(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := testConfirmations(clock)
	// Чат шлёт команды и отменяет их быстрее, чем подтверждения забываются:
	// память чата не растёт сверх предела.
	cancelAll := func(n int) {
		for range n {
			id, err := c.add(testChat, (&countedAction{}).run)
			if err != nil {
				t.Fatal(err)
			}
			c.press(testChat, id, false)
			clock.Advance(time.Second)
		}
	}
	cancelAll(4 * telegramConfirmKeptPerChat)
	if n := c.size(); n > telegramConfirmKeptPerChat {
		t.Fatalf("%d confirmations of one chat kept, want at most %d", n, telegramConfirmKeptPerChat)
	}
	// Идущее действие не забывается: повтор нажатия узнаёт, что оно идёт.
	running, err := c.add(testChat, (&countedAction{}).run)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.press(testChat, running, true); got.run == nil {
		t.Fatalf("first press = %+v", got)
	}
	cancelAll(2 * telegramConfirmKeptPerChat)
	if got := c.press(testChat, running, true); got.answer != "This action is already running." {
		t.Fatalf("press of the running confirmation = %+v", got)
	}
	// Предел — на чат: чужой чат свои подтверждения не теряет.
	other := telegramChat{userID: "u2", chatID: 99, session: "session-9"}
	kept, err := c.add(other, (&countedAction{}).run)
	if err != nil {
		t.Fatal(err)
	}
	for range telegramConfirmKeptPerChat {
		if _, err := c.add(testChat, (&countedAction{}).run); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.press(other, kept, true); got.run == nil {
		t.Fatalf("another chat's confirmations evicted this one: %+v", got)
	}
}

// Кнопка другого вида (ответ на запрос, #283) — не подтверждение: нажатие
// получает отказ, а её клавиатура остаётся.
func TestTelegramPressOfAnotherButtonKeepsItsKeyboard(t *testing.T) {
	clock := &telegramClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	c := &telegramCommands{confirm: testConfirmations(clock)}
	for _, data := range []string{"a:17:0", "s:17", "x", ""} {
		if got := c.press(testChat, data); got.run != nil || got.clear || got.answer != "This button is no longer valid; nothing was done." {
			t.Fatalf("press of %q = %+v", data, got)
		}
	}
	// Испорченное подтверждение — своя кнопка: клавиатура снимается.
	if got := c.press(testChat, "c:unknown:y"); got.run != nil || !got.clear {
		t.Fatalf("press of an unknown confirmation = %+v", got)
	}
}
