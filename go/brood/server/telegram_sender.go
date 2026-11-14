package server

// Отправитель уведомлений Telegram (#282, эпик #278). Уведомление пишет в
// outbox транзакция доменного изменения (store.queueTelegram), а
// отправитель после коммита берёт очередь каждого пользователя: свой
// работник на пользователя, строго по порядку id и одна отправка за раз.
// Попытка отмечается в базе до sendMessage, исход записывается после:
//   - Telegram принял сообщение — sent с номером сообщения: его правит ответ
//     на запрос (#283);
//   - запрос заведомо не ушёл (нет соединения, 429) — та же строка снова
//     голова очереди, после паузы, сколько бы попыток ни было: порядок
//     очереди держит её, пока Telegram не ответит; failed — только
//     уведомление старше telegramSendMaxAge;
//   - явный отказ — failed, очередь идёт дальше;
//   - исход неизвестен (ответ потерян, 5xx) — unconfirmed: сообщение могло
//     дойти, и отправитель его не повторяет; повтор только явный.
//
// Исход, который работник узнал, не теряется: отказ его записи повторяется,
// пока сервер жив, и ещё раз при остановке. Остановка сервера не обрывает
// отправку в полёте сразу: та получает telegramSendGrace, и её исход
// записан до закрытия базы; новую отправку остановка не начинает.
//
// Доставка at-least-once только в смысле эпика: отметку попытки может
// откатить потеря питания (synchronous=NORMAL), и тогда сообщение уйдёт
// снова. Набор работников сверяется с очередями базы по таймеру. Пакет
// молчит: исход виден в outbox.
//
// После ответа на запрос отправитель правит его отправленные сообщения
// (requestClosed, #283): кнопки ответа уступают кнопке состояния. Правка —
// лучшая попытка без outbox и повтора: сообщение, которое Telegram править
// не даёт, — не ошибка.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

const (
	// telegramScanInterval — как часто отправитель ищет пользователей с
	// очередью: столько ждёт уведомление, поставленное без работника.
	telegramScanInterval = 2 * time.Second
	// telegramSendAttempts — сколько попыток получает уведомление, которое не
	// дошло до отправки из-за сбоя чтения базы, прежде чем стать failed: паузы
	// backoff поллеров (1, 2, 4 … 60 секунд) дают около двух минут. Счёт —
	// все попытки уведомления. Отправку, которая заведомо не ушла, предел не
	// ограничивает: её ограничивает возраст, telegramSendMaxAge.
	telegramSendAttempts = 8
	// telegramSendMaxAge — возраст, после которого уведомление, которое снова
	// заведомо не ушло, становится failed. Пока Telegram недостижим, голова
	// держит очередь пользователя без предела попыток, а сбой дольше суток не
	// копит в очереди больше событий этих суток.
	telegramSendMaxAge = 24 * time.Hour
	// telegramRetention — сколько хранится уведомление с записанным
	// исходом: номер сообщения нужен правке кнопок (#283), а без срока
	// таблица росла бы без предела.
	telegramRetention = 30 * 24 * time.Hour
	// telegramPruneInterval — как часто отправитель удаляет уведомления
	// старше срока хранения.
	telegramPruneInterval = time.Hour
	// telegramSettleTimeout ограничивает одну запись исхода: её не отменяет
	// остановка сервера, иначе отправленное сообщение осталось бы попыткой
	// в полёте.
	telegramSettleTimeout = 10 * time.Second
	// telegramSendGrace — сколько остановка сервера ждёт отправку в полёте:
	// успевшая отправка записывается sent с номером сообщения, а не
	// unconfirmed после перезапуска. Дольше — отправка обрывается, и её
	// исход неизвестен.
	telegramSendGrace = 5 * time.Second
)

// telegramSender — отправитель outbox и его работники по пользователям.
type telegramSender struct {
	store  *store.Store
	client telegram.Config
	// install — токен бота установки, пусто — его нет.
	install string
	// panel — origin панели для ссылок, пусто — без ссылок.
	panel string
	opts  telegramOptions

	mu      sync.Mutex
	workers map[string]bool
	wg      sync.WaitGroup
	// ctx — время жизни сервера для правок сообщений; closed — остановка
	// началась, новых правок нет; editing ждёт начатые.
	ctx     context.Context
	closed  bool
	editing sync.WaitGroup
}

func newTelegramSender(st *store.Store, cfg Config, opts telegramOptions) *telegramSender {
	return &telegramSender{
		store:   st,
		client:  telegram.Config{BaseURL: cfg.TelegramAPI},
		install: cfg.telegramBotToken,
		panel:   cfg.PanelOrigin,
		opts:    opts.withDefaults(),
		workers: map[string]bool{},
	}
}

// start привязывает отправитель ко времени жизни сервера. Прерванные
// попытки прежнего процесса восстановление уже записало unconfirmed
// (newFromStore), до первой попытки этого.
func (t *telegramSender) start(ctx context.Context) {
	t.mu.Lock()
	t.ctx = ctx
	t.mu.Unlock()
	t.wg.Add(1)
	go t.run(ctx)
}

// close ждёт работников и правки сообщений: исход последней попытки записан
// до закрытия базы, и правка базу уже не читает.
func (t *telegramSender) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.wg.Wait()
	t.editing.Wait()
}

func (t *telegramSender) run(ctx context.Context) {
	defer t.wg.Done()
	ticker := time.NewTicker(t.opts.scan)
	defer ticker.Stop()
	var pruned time.Time
	for {
		if time.Since(pruned) >= t.opts.prune {
			// Не удалось — удалит следующий проход.
			_, _ = t.store.PruneTelegramOutbox(ctx, time.Now().Add(-t.opts.retention))
			pruned = time.Now()
		}
		t.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// dispatch запускает работника каждому пользователю с очередью, у которого
// его ещё нет. Работник уходит, когда очередь пуста; поставленное после
// этого берёт работник следующей сверки.
func (t *telegramSender) dispatch(ctx context.Context) {
	users, err := t.store.TelegramOutboxUsers(ctx)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, user := range users {
		if t.workers[user] || ctx.Err() != nil {
			continue
		}
		t.workers[user] = true
		t.wg.Add(1)
		go t.work(ctx, user)
	}
}

// work отправляет очередь пользователя по порядку, одно уведомление за раз.
func (t *telegramSender) work(ctx context.Context, userID string) {
	defer t.wg.Done()
	defer func() {
		t.mu.Lock()
		delete(t.workers, userID)
		t.mu.Unlock()
	}()
	bots := telegramBotClients{config: t.client}
	defer bots.close()
	failures := 0
	for ctx.Err() == nil {
		n, err := t.store.BeginTelegramAttempt(ctx, userID, t.install != "")
		if errors.Is(err, store.ErrTelegramUndeliverable) {
			continue
		}
		if err != nil {
			// Очередь пуста, или база не ответила: следующая сверка запустит
			// работника снова.
			return
		}
		outcome, wait := t.attempt(ctx, &bots, n)
		if err = t.settle(ctx, n.ID, outcome); err != nil {
			// Исход не записался и при остановке — попытка в полёте, её
			// запишет восстановление при следующем запуске, а повторить её
			// ограждение не даст; или попытку уже закрыл другой.
			return
		}
		if outcome.State == "sent" && telegramOpensRequest(n.Kind) {
			// Ответ мог прийти, пока сообщение было в полёте: правка после
			// ответа его ещё не видела отправленным. Открытый запрос правка
			// не трогает.
			t.editLater(n.Event.Swarm, n.Event.Request, n.ID)
		}
		if !outcome.Requeue {
			failures = 0
			continue
		}
		failures++
		if !sleepContext(ctx, max(t.opts.backoff(failures), wait)) {
			return
		}
	}
}

// attempt отправляет уведомление и возвращает исход попытки и наименьшую
// паузу до следующей (retry_after отказа 429).
func (t *telegramSender) attempt(ctx context.Context, bots *telegramBotClients, n store.TelegramNotification) (store.TelegramOutcome, time.Duration) {
	token := t.install
	if n.OwnBot {
		var err error
		token, err = t.store.TelegramBotToken(ctx, n.UserID, "")
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Свой бот снят после начала попытки: слать нечем.
			return store.TelegramOutcome{State: "failed"}, 0
		case err != nil:
			return t.unread(ctx, n), 0
		}
	}
	notice, err := t.notice(ctx, n)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Объект события исчез (рой удалён): сообщать не о чем.
		return store.TelegramOutcome{State: "failed"}, 0
	case err != nil:
		return t.unread(ctx, n), 0
	}
	client, err := bots.client(token)
	if err != nil {
		return store.TelegramOutcome{State: "failed"}, 0
	}
	send, cancel := telegramSendContext(ctx, t.opts.grace)
	defer cancel()
	m, err := notice.send(send, client, n.ChatID)
	return telegramSendOutcome(m, err, telegramStale(n.CreatedAt, t.opts.now()))
}

// unread — исход попытки, которую сбой чтения базы оставил без отправки:
// снова в очередь, пока у уведомления меньше telegramSendAttempts попыток.
// Чтение, которое оборвала остановка сервера, — не сбой уведомления: оно
// снова в очереди и за пределом, который могли выбрать отправки, заведомо не
// ушедшие за долгий сбой сети.
func (t *telegramSender) unread(ctx context.Context, n store.TelegramNotification) store.TelegramOutcome {
	return store.TelegramOutcome{State: "failed", Requeue: ctx.Err() != nil || n.Attempts < int64(t.opts.sendAttempts)}
}

// telegramStale: уведомление, поставленное в created (RFC 3339 хранилища),
// к моменту now старше telegramSendMaxAge. Время, которое не прочесть, —
// старше: строку записали мимо хранилища.
func telegramStale(created string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, created)
	return err != nil || now.Sub(at) > telegramSendMaxAge
}

// telegramSendContext — контекст отправки уведомления. Остановка сервера
// (ctx) обрывает отправку в полёте не сразу, а через grace; отправку, которую
// остановка застала до начала, контекст не начинает (ErrNotSent клиента).
func telegramSendContext(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	send, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if ctx.Err() != nil {
		cancel()
		return send, cancel
	}
	stop := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-send.Done():
		}
	})
	return send, func() {
		stop()
		cancel()
	}
}

// settle записывает исход попытки, который работник уже знает. Отказ базы
// не оставляет попытку в полёте: пока сервер жив, запись повторяется после
// паузы backoff, а остановка, заставшая паузу, оставляет ей одну последнюю
// попытку. Запись не отменяет остановка, её срок — telegramSettleTimeout.
// Повтор кончают отказы, которых он не исправит: попытки в полёте нет (её
// закрыл другой) и неверный исход.
func (t *telegramSender) settle(ctx context.Context, id int64, outcome store.TelegramOutcome) error {
	for failures := 0; ; failures++ {
		if failures > 0 {
			sleepContext(ctx, t.opts.backoff(failures))
		}
		write, cancel := context.WithTimeout(context.WithoutCancel(ctx), telegramSettleTimeout)
		err := t.store.SettleTelegramAttempt(write, id, outcome)
		cancel()
		if err == nil || errors.Is(err, store.ErrTelegramNoAttempt) || errors.Is(err, store.ErrTelegramOutcomeInvalid) || ctx.Err() != nil {
			return err
		}
	}
}

// telegramSendOutcome — исход попытки по ответу клиента: отправлено,
// неизвестно, заведомо не ушло (снова в очередь, если уведомление не stale)
// или отказано. Пауза — retry_after отказа 429, ноль у прочих.
func telegramSendOutcome(m telegram.Message, err error, stale bool) (store.TelegramOutcome, time.Duration) {
	var refused *telegram.Error
	switch {
	case err == nil:
		return store.TelegramOutcome{State: "sent", MessageID: m.MessageID}, 0
	case errors.Is(err, telegram.ErrUncertain):
		return store.TelegramOutcome{State: "unconfirmed"}, 0
	case errors.Is(err, telegram.ErrNotSent):
		return store.TelegramOutcome{State: "failed", Requeue: !stale}, 0
	case errors.As(err, &refused) && refused.Code == http.StatusTooManyRequests:
		return store.TelegramOutcome{State: "failed", Requeue: !stale}, refused.RetryAfter
	}
	return store.TelegramOutcome{State: "failed"}, 0
}

// notice читает объекты события и собирает сообщение.
func (t *telegramSender) notice(ctx context.Context, n store.TelegramNotification) (telegramNotice, error) {
	f, err := t.facts(ctx, n.Kind, n.Event)
	if err != nil {
		return telegramNotice{}, err
	}
	return renderTelegramNotice(n, f, t.panel), nil
}

// facts читает рой события, его шаг (задача начатого прохода, причина отказа
// шага), его запрос, спросившего актора и автора ответа. Удалённый актор или
// автор называется своим id.
func (t *telegramSender) facts(ctx context.Context, kind string, ev store.TelegramEvent) (telegramFacts, error) {
	w, err := t.store.GetSwarm(ctx, ev.Swarm)
	if err != nil {
		return telegramFacts{}, err
	}
	f := telegramFacts{swarm: w.Name, definition: w.DefinitionName}
	if kind == "pass_started" || kind == "step_failed" {
		step, err := t.store.GetSwarmStep(ctx, ev.Swarm, ev.Sequence)
		if err != nil {
			return telegramFacts{}, err
		}
		f.step = &step
	}
	if ev.Request == "" {
		return f, nil
	}
	req, err := t.store.GetSwarmRequest(ctx, ev.Swarm, ev.Request)
	if err != nil {
		return telegramFacts{}, err
	}
	f.request = &req
	if req.ActorUID != "" {
		f.actor = req.ActorUID
		if a, err := t.store.GetManagedActor(ctx, req.ActorUID); err == nil && a.Name != "" {
			f.actor = a.Name
		}
	}
	if req.AnsweredBy != "" {
		f.answeredBy = telegramUserName(ctx, t.store, req.AnsweredBy)
	}
	return f, nil
}

// telegramUserName — имя пользователя для чата: отображаемое имя, без него
// логин; удалённый пользователь называется своим id.
func telegramUserName(ctx context.Context, st *store.Store, userID string) string {
	u, err := st.GetUser(ctx, userID)
	switch {
	case err != nil:
		return userID
	case u.DisplayName != nil && *u.DisplayName != "":
		return *u.DisplayName
	}
	return u.Username
}

// send отправляет сообщение уведомления в чат одним сообщением Bot API:
// текстом или документом, с кнопками ответа, если они есть.
func (n telegramNotice) send(ctx context.Context, c *telegram.Client, chatID int64) (telegram.Message, error) {
	if n.doc != nil {
		return c.SendDocument(ctx, chatID, *n.doc, n.markup)
	}
	return c.SendMessage(ctx, chatID, n.text, n.markup)
}

// telegramOpensRequest: уведомление вида kind открывает запрос и несёт его
// кнопки.
func telegramOpensRequest(kind string) bool {
	return kind == "request_opened" || kind == "actor_question"
}

// requestClosed правит после ответа сообщения запроса: кнопки ответа
// уступают кнопке состояния (editRequest). Правка идёт в своей горутине и
// ответа не задерживает.
func (t *telegramSender) requestClosed(swarmID, requestID string) {
	t.editLater(swarmID, requestID, 0)
}

// editLater начинает правку сообщений запроса; only — правка одного
// уведомления, 0 — всех. После начала остановки правок нет.
func (t *telegramSender) editLater(swarmID, requestID string, only int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctx == nil || t.closed || t.ctx.Err() != nil {
		return
	}
	t.editing.Add(1)
	go func(ctx context.Context) {
		defer t.editing.Done()
		t.editRequest(ctx, swarmID, requestID, only)
	}(t.ctx)
}

// editRequest заменяет кнопки отправленных сообщений закрытого запроса
// кнопкой состояния: кто и когда ответил или что запрос отменён. Открытый
// запрос не правится. Сообщения выбирает хранилище: отправленные в чат
// нынешней привязки получателя (TelegramRequestMessages), с кнопками или
// без, — кнопки получил тот, кто управлял роем в момент отправки, а не
// сейчас. Кнопку состояния получает тот, кто читает рой сейчас, и viewer
// тоже; у того, кто рой больше не читает, кнопки ответа снимаются без
// состояния. Отказ Telegram (и «не изменено» у сообщения без кнопок), сбой
// сети и сбой чтения базы правку этого сообщения пропускают без повтора: это
// лучшая попытка, а нажатие на оставшуюся кнопку покажет состояние запроса и
// поправит её.
func (t *telegramSender) editRequest(ctx context.Context, swarmID, requestID string, only int64) {
	req, err := t.store.GetSwarmRequest(ctx, swarmID, requestID)
	if err != nil || req.State == "open" {
		return
	}
	messages, err := t.store.TelegramRequestMessages(ctx, swarmID, requestID)
	if err != nil {
		return
	}
	state := telegramRequestState(req, telegramUserName(ctx, t.store, req.AnsweredBy))
	bots := telegramBotClients{config: t.client}
	defer bots.close()
	for _, m := range messages {
		if only != 0 && m.ID != only {
			continue
		}
		token := t.install
		if m.OwnBot {
			if token, err = t.store.TelegramBotToken(ctx, m.UserID, ""); err != nil {
				continue
			}
		}
		client, err := bots.client(token)
		if err != nil {
			continue
		}
		var markup *telegram.InlineKeyboardMarkup
		if m.Reader {
			markup = telegramStatusKeyboard(m.ID, state)
		}
		_ = client.EditMessageReplyMarkup(ctx, m.ChatID, m.MessageID, markup)
	}
}

// telegramBotClients — клиент Bot API работника: один на токен, новый при
// смене токена. Токен клиент держит сам, работник — только отпечаток.
type telegramBotClients struct {
	config      telegram.Config
	current     *telegram.Client
	fingerprint [32]byte
}

func (b *telegramBotClients) client(token string) (*telegram.Client, error) {
	fingerprint := telegramFingerprint(token)
	if b.current != nil && fingerprint == b.fingerprint {
		return b.current, nil
	}
	b.close()
	client, err := telegram.NewClient(token, b.config)
	if err != nil {
		return nil, err
	}
	b.current, b.fingerprint = client, fingerprint
	return client, nil
}

func (b *telegramBotClients) close() {
	if b.current != nil {
		b.current.Close()
		b.current = nil
	}
}
