package server

// Подтверждения команд Telegram (#284). /start, /stop и /actor исполняются
// только после нажатия кнопки. Подтверждение принадлежит одному привязанному
// чату — пользователю, боту и чату под сессией его привязки — и живёт в
// памяти процесса: перезапуск сервера его забывает, и нажатие после
// перезапуска лишь недействительно, как и нажатие чата, привязанного с тех
// пор заново. Так подтверждение никогда не исполняется само: срок, отмена,
// перезапуск, новая привязка — всё ведёт к «ничего не сделано».
//
// Нажатие исполняет действие не больше одного раза. Первое подтверждающее
// нажатие в срок переводит подтверждение в running и забирает действие;
// всякое следующее, и тот же callback, который Telegram прислал снова, и
// второе нажатие, узнаёт только, что действие идёт или прошло. Переход —
// под одной блокировкой, поэтому два поллера (свой бот и бот установки) или
// два нажатия подряд действия не удваивают. Срок держит и начало действия:
// забранное в срок оно начинается не позже telegramConfirmGrace после
// срока (begin), иначе подтверждение истекает.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	// telegramConfirmTTL — срок подтверждения от его выдачи: нажатие в этот
	// момент и позже действие не исполняет.
	telegramConfirmTTL = 2 * time.Minute
	// telegramConfirmGrace — сколько после срока может начаться действие
	// нажатия, принятого в срок. Ответ нажатию и снятие клавиатуры идут до
	// действия, каждый до 30 секунд (срок клиента Bot API): без предела
	// нажатие за секунду до срока начинало бы запись почти через минуту после
	// него. Действие, не начавшееся к сроку и отсрочке, не исполняется.
	telegramConfirmGrace = 10 * time.Second
	// telegramConfirmKeep — сколько подтверждение помнится после срока:
	// повтор нажатия в это время узнаёт исход, а позже лишь недействителен.
	telegramConfirmKeep = 10 * time.Minute
	// telegramConfirmPerChat — открытых подтверждений на чат: новое сверх
	// предела истекает самое старое.
	telegramConfirmPerChat = 8
	// telegramConfirmKeptPerChat — подтверждений чата в памяти в любом
	// состоянии: новое сверх предела забывает самое старое, кроме идущего.
	// Решённые подтверждения помнятся telegramConfirmKeep после срока, и без
	// этого предела чат, который шлёт команды и отменяет их, растил бы
	// память процесса со скоростью своих команд.
	telegramConfirmKeptPerChat = 32
	// telegramConfirmIDBytes — случайные байты id: 96 бит, 16 знаков
	// base64url. callback_data — «c:<id>:y» или «:n», 20 байт из 64.
	telegramConfirmIDBytes = 12
	// telegramConfirmPrefix — вид callback_data подтверждения; другие виды
	// кнопок (ответ на запрос, #283) берут свой префикс.
	telegramConfirmPrefix = "c:"
)

// Ответы на нажатие (answerCallbackQuery, до 200 знаков).
const (
	telegramPressRunning      = "Running."
	telegramPressBusy         = "This action is already running."
	telegramPressRan          = "This action already ran; it is not repeated."
	telegramPressCancel       = "Cancelled; nothing was done."
	telegramPressWasCancelled = "This confirmation was cancelled; nothing was done."
	telegramPressExpired      = "This confirmation expired; nothing was done."
	telegramPressInvalid      = "This button is no longer valid; nothing was done."
)

// telegramChat — привязанный чат, от имени которого идёт команда:
// пользователь brood, бот обновления (имя своего бота, пусто — бот
// установки), личный чат, чей id равен id собеседника, и сессия telegram
// привязки, под которой пришло обновление (store.TelegramChatSession).
// Чат, отвязанный и привязанный снова, — другой чат: у него другая сессия.
type telegramChat struct {
	userID      string
	botUsername string
	chatID      int64
	session     string
}

// linked: обновление пришло под живой привязкой.
func (c telegramChat) linked() bool { return c.session != "" }

// telegramConfirmState — состояние подтверждения.
type telegramConfirmState int

const (
	confirmPending telegramConfirmState = iota
	confirmRunning
	confirmDone
	confirmCancelled
	confirmExpired
)

// telegramConfirmation — ждущее нажатия действие одного чата.
type telegramConfirmation struct {
	chat    telegramChat
	created time.Time
	expires time.Time
	state   telegramConfirmState
	run     func(ctx context.Context) string
}

// telegramPress — что сделать с нажатием: ответ answerCallbackQuery,
// снять ли клавиатуру сообщения и действие, которое нажатие забрало (nil —
// ничего не исполнять).
type telegramPress struct {
	answer string
	clear  bool
	run    func(ctx context.Context) string
}

// telegramConfirmations — подтверждения всех чатов сервера.
type telegramConfirmations struct {
	mu      sync.Mutex
	now     func() time.Time
	random  io.Reader
	entries map[string]*telegramConfirmation
}

func newTelegramConfirmations(now func() time.Time) *telegramConfirmations {
	return &telegramConfirmations{now: now, random: rand.Reader, entries: map[string]*telegramConfirmation{}}
}

// add заводит подтверждение действия run для чата и возвращает его id.
// Сверх telegramConfirmPerChat открытых подтверждений чата самое старое
// истекает, сверх telegramConfirmKeptPerChat подтверждений чата самое старое
// не идущее забывается; подтверждения после срока хранения удаляются.
func (c *telegramConfirmations) add(chat telegramChat, run func(context.Context) string) (string, error) {
	var random [telegramConfirmIDBytes]byte
	if _, err := io.ReadFull(c.random, random[:]); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(random[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneLocked(now)
	if oldest, n := c.oldestLocked(chat, func(e *telegramConfirmation) bool { return e.state != confirmRunning }); n >= telegramConfirmKeptPerChat {
		delete(c.entries, oldest)
	}
	if oldest, n := c.oldestLocked(chat, func(e *telegramConfirmation) bool { return e.state == confirmPending }); n >= telegramConfirmPerChat {
		c.entries[oldest].state = confirmExpired
	}
	c.entries[id] = &telegramConfirmation{chat: chat, created: now, expires: now.Add(telegramConfirmTTL), state: confirmPending, run: run}
	return id, nil
}

// oldestLocked — id самого старого подтверждения чата из тех, что отбирает
// counted, и их число.
func (c *telegramConfirmations) oldestLocked(chat telegramChat, counted func(*telegramConfirmation) bool) (string, int) {
	var oldest string
	n := 0
	for id, e := range c.entries {
		if e.chat != chat || !counted(e) {
			continue
		}
		n++
		if oldest == "" || e.created.Before(c.entries[oldest].created) {
			oldest = id
		}
	}
	return oldest, n
}

// press решает, что делает нажатие чата chat на подтверждение id: yes —
// кнопка подтверждения, иначе отмены. Действие забирает только первое
// подтверждающее нажатие открытого подтверждения в срок.
func (c *telegramConfirmations) press(chat telegramChat, id string, yes bool) telegramPress {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneLocked(now)
	e, ok := c.entries[id]
	if !ok || e.chat != chat {
		return telegramPress{answer: telegramPressInvalid, clear: true}
	}
	switch e.state {
	case confirmRunning:
		return telegramPress{answer: telegramPressBusy}
	case confirmDone:
		return telegramPress{answer: telegramPressRan}
	case confirmCancelled:
		return telegramPress{answer: telegramPressWasCancelled, clear: true}
	case confirmExpired:
		return telegramPress{answer: telegramPressExpired, clear: true}
	}
	if !now.Before(e.expires) {
		e.state = confirmExpired
		return telegramPress{answer: telegramPressExpired, clear: true}
	}
	if !yes {
		e.state = confirmCancelled
		return telegramPress{answer: telegramPressCancel, clear: true}
	}
	e.state = confirmRunning
	return telegramPress{answer: telegramPressRunning, clear: true, run: e.run}
}

// begin: действие подтверждения id, которое забрало нажатие, ещё может
// начаться — до срока и telegramConfirmGrace после него. Позже подтверждение
// истекает, ничего не исполнив: повтор нажатия узнаёт, что оно истекло.
func (c *telegramConfirmations) begin(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok || e.state != confirmRunning {
		return false
	}
	if !c.now().Before(e.expires.Add(telegramConfirmGrace)) {
		e.state = confirmExpired
		e.run = nil
		return false
	}
	return true
}

// finish отмечает, что действие подтверждения id кончилось, с любым исходом:
// повтор нажатия его уже не исполнит. Подтверждение, которое истекло до
// начала действия (begin), остаётся истёкшим.
func (c *telegramConfirmations) finish(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[id]; ok && e.state == confirmRunning {
		e.state = confirmDone
		e.run = nil
	}
}

// size — сколько подтверждений помнит сервер.
func (c *telegramConfirmations) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// pruneLocked забывает подтверждения, чей срок вышел дольше
// telegramConfirmKeep назад, в любом состоянии.
func (c *telegramConfirmations) pruneLocked(now time.Time) {
	for id, e := range c.entries {
		if !now.Before(e.expires.Add(telegramConfirmKeep)) {
			delete(c.entries, id)
		}
	}
}

// telegramConfirmData — callback_data кнопки подтверждения (yes) или отмены.
func telegramConfirmData(id string, yes bool) string {
	choice := "n"
	if yes {
		choice = "y"
	}
	return telegramConfirmPrefix + id + ":" + choice
}

// parseTelegramConfirm разбирает callback_data кнопки подтверждения.
func parseTelegramConfirm(data string) (id string, yes, ok bool) {
	rest, found := strings.CutPrefix(data, telegramConfirmPrefix)
	if !found {
		return "", false, false
	}
	id, choice, found := strings.Cut(rest, ":")
	if !found || id == "" || (choice != "y" && choice != "n") {
		return "", false, false
	}
	return id, choice == "y", true
}
