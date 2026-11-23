package server

// Поллеры Telegram сервера (#281, эпик #278). Сервер снаружи недостижим, и
// обновления ботов он забирает long polling getUpdates. Telegram отдаёт
// очередь бота одному getUpdates, поэтому поллер — на бота, а не на
// пользователя: один на свой бот каждого включённого пользователя, чья
// привязка не в error и чей бот не бот установки, и один на бот установки
// (BROOD_TELEGRAM_BOT_TOKEN), общий для пользователей без своего бота. Набор
// поллеров сверяет с базой sync: на старте, после записи профиля и по
// таймеру. Так блокировка и отзыв сессий, которые пишет auth, останавливают
// поллер без связи с ним.
//
// Поллер принимает код привязки из личного чата и передаёт сообщения
// привязанного чата в шину роя. Исход поллера виден в профиле.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

const (
	// telegramReconcileInterval — как часто набор поллеров сверяется с базой:
	// столько живёт поллер отключённого пользователя или отозванной сессии.
	telegramReconcileInterval = 15 * time.Second
	// telegramBackoffMin и telegramBackoffMax — пауза после сбоя getUpdates:
	// от секунды, вдвое за сбой подряд, не дольше минуты.
	telegramBackoffMin = time.Second
	telegramBackoffMax = time.Minute
	// telegramLinkCodeTTL — срок кода привязки от выдачи; хранилище не даёт
	// больше часа.
	telegramLinkCodeTTL = 15 * time.Minute
	// telegramLinkCodeBytes — случайные байты кода: 256 бит, в base64url без
	// дополнения 43 знака, в пределах 64 знаков параметра deep link t.me.
	telegramLinkCodeBytes = 32
	// telegramFailTimeout ограничивает запись ошибки привязки остановленным
	// поллером.
	telegramFailTimeout = 10 * time.Second
)

// errTelegramStopped — сверка после остановки поллеров сервера.
var errTelegramStopped = errors.New("telegram pollers are stopped")

// Ответы поллера в чат. Отказ кода один на все причины и не называет
// аккаунт: чат, приславший код, ничего не узнаёт о его владельце.
const (
	telegramReplyLinked      = "This chat is now linked to your brood account."
	telegramReplyCodeRefused = "This link code is invalid or has expired. Issue a new code in your brood profile and send it here with /start."
	telegramReplyChatTaken   = "This chat is linked to another brood account. Unlink it there first."
	telegramReplyLinkFailed  = "brood could not link this chat right now. Try again later."
	telegramReplyStart       = "To link this chat, issue a link code in your brood profile and send it here as /start <code>."
)

// Причины ошибки привязки, которые пишут поллер и сверка. К причинам 401 и
// 409 добавляется текст отказа клиента, токен клиент в нём уже замаскировал.
const (
	telegramReasonUnauthorized = "Telegram rejected the bot token (401): set a new bot token"
	telegramReasonConflict     = "another getUpdates poller or a webhook uses this bot (409): stop it, then issue a new link code"
	// telegramReasonInstallationBot — свой бот пользователя стал ботом
	// установки; его опрашивает только поллер установки.
	telegramReasonInstallationBot = "this bot is now the installation bot: remove it as your own bot or set another bot token"
)

// telegramOptions — ритм поллеров, часы и источник случайности кода;
// нулевые поля берут умолчания.
type telegramOptions struct {
	// wait — long poll getUpdates, целые секунды до telegram.MaxPollWait.
	wait time.Duration
	// reconcile — период сверки набора поллеров с базой.
	reconcile time.Duration
	// backoff — пауза после failures сбоев подряд.
	backoff func(failures int) time.Duration
	// now — часы выдачи и подтверждения кода и возраста уведомления
	// (telegramSendMaxAge).
	now func() time.Time
	// random — источник байтов кода привязки, в работе crypto/rand.
	random io.Reader
	// beforeWrite зовёт API профиля после middleware, перед транзакцией
	// записи; nil в работе. В ней проверки отзывают сессию запроса.
	beforeWrite func(r *http.Request)
	// afterWrite зовёт API профиля после фиксации записи, перед сверкой
	// набора поллеров; nil в работе. В ней проверки уводят клиента.
	afterWrite func(r *http.Request)
	// reconciling зовёт desired сразу после чтения базы, до снимков памяти
	// сверки; nil в работе. В ней проверки привязывают чат: снимок, взятый
	// после чтения, эту привязку уже видит.
	reconciling func()
}

func (o telegramOptions) withDefaults() telegramOptions {
	if o.wait == 0 {
		o.wait = telegram.MaxPollWait
	}
	if o.reconcile == 0 {
		o.reconcile = telegramReconcileInterval
	}
	if o.backoff == nil {
		o.backoff = telegramBackoff
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.random == nil {
		o.random = rand.Reader
	}
	return o
}

// writing отмечает переход запроса записи профиля к транзакции.
func (o telegramOptions) writing(r *http.Request) {
	if o.beforeWrite != nil {
		o.beforeWrite(r)
	}
}

// written отмечает фиксацию записи профиля перед сверкой поллеров.
func (o telegramOptions) written(r *http.Request) {
	if o.afterWrite != nil {
		o.afterWrite(r)
	}
}

// telegramBackoff — пауза после failures сбоев подряд.
func telegramBackoff(failures int) time.Duration {
	d := telegramBackoffMin
	for i := 1; i < failures && d < telegramBackoffMax; i++ {
		d *= 2
	}
	return min(d, telegramBackoffMax)
}

// telegramBots — поллеры ботов сервера и то, что они знают в памяти.
type telegramBots struct {
	store  *store.Store
	client telegram.Config
	// install — токен бота установки, пусто — его нет; installID — id бота
	// из токена.
	install   string
	installID int64
	opts      telegramOptions
	// channel принимает сообщения привязанных чатов и отправляет ответы
	// агентов; его заводит сборка сервера до старта поллеров.
	channel *telegramChannel

	// syncMu сериализует sync и останов: набор поллеров меняет только
	// держатель. Поллеры его не берут.
	syncMu  sync.Mutex
	ctx     context.Context
	pollers map[string]*telegramPoller

	// mu держит то, что поллеры пишут сами.
	mu sync.Mutex
	// offsets — offset getUpdates по id бота: перезапуск поллера в этом
	// процессе продолжает очередь, а не берёт её с начала.
	offsets map[int64]int64
	// installChats — чаты, привязанные через бот установки, и их
	// пользователи; права перепроверяет TelegramSessionActive. installLinks —
	// счёт привязок, которые подтвердил поллер (telegramInstallChat).
	installChats map[int64]telegramInstallChat
	installLinks uint64
	// installBot — имя бота установки из getMe, пусто — ещё не известно;
	// installFailure — причина его остановки (401, 409).
	installBot     string
	installFailure string
	// halted — отпечаток токена, чей поллер своего бота остановлен 401 или
	// 409, по пользователю. Сверка его не запускает, даже если ошибку
	// привязки записать не удалось; снимают его новый токен и новый код.
	halted map[string][32]byte

	kick chan struct{}
	wg   sync.WaitGroup
}

// telegramInstallChat — пользователь чата бота установки и номер привязки,
// которую подтвердил поллер; у чата из базы номера нет. Чат с номером сверка
// снимает только набором, прочитанным после этой привязки: сверка,
// прочитавшая базу до неё, чат не теряет (#300).
type telegramInstallChat struct {
	userID string
	link   uint64
}

func newTelegramBots(st *store.Store, cfg Config, opts telegramOptions) *telegramBots {
	return &telegramBots{
		store:        st,
		client:       telegram.Config{BaseURL: cfg.TelegramAPI},
		install:      cfg.telegramBotToken,
		installID:    telegramBotID(cfg.telegramBotToken),
		opts:         opts.withDefaults(),
		pollers:      map[string]*telegramPoller{},
		offsets:      map[int64]int64{},
		installChats: map[int64]telegramInstallChat{},
		halted:       map[string][32]byte{},
		kick:         make(chan struct{}, 1),
	}
}

// telegramBotID — id бота из токена BotFather (<id>:<секрет>), 0 — не токен.
func telegramBotID(token string) int64 {
	head, _, ok := strings.Cut(token, ":")
	if !ok {
		return 0
	}
	id, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// newTelegramLinkCode — одноразовый код привязки: telegramLinkCodeBytes
// байтов source в base64url. Источник в работе — crypto/rand
// (telegramOptions.random); короткое чтение — отказ, а не короткий код.
func newTelegramLinkCode(source io.Reader) (string, error) {
	var random [telegramLinkCodeBytes]byte
	if _, err := io.ReadFull(source, random[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random[:]), nil
}

// start привязывает поллеры ко времени жизни сервера: первая сверка
// запускает поллеры существующих ботов.
func (b *telegramBots) start(ctx context.Context) {
	b.syncMu.Lock()
	b.ctx = ctx
	b.syncMu.Unlock()
	b.wg.Add(1)
	go b.run(ctx)
}

func (b *telegramBots) run(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.opts.reconcile)
	defer ticker.Stop()
	for {
		// Несверенный набор ждёт следующего тика или просьбы.
		_ = b.sync(ctx)
		select {
		case <-ctx.Done():
			b.stopAll()
			return
		case <-ticker.C:
		case <-b.kick:
		}
	}
}

// close ждёт остановки всех поллеров и доставок ответов, которые они
// начали; время жизни отменил Server.Close. Доставку начинает только живой
// поллер, а поллеры к этому моменту остановлены.
func (b *telegramBots) close() {
	if b != nil {
		b.wg.Wait()
	}
}

// nudge просит сверку, не дожидаясь её.
func (b *telegramBots) nudge() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// telegramBotSpec — бот, которому нужен поллер. userID пуст у бота
// установки; botUsername — имя своего бота из getMe, у бота установки пусто,
// как его называет хранилище.
type telegramBotSpec struct {
	userID      string
	token       string
	botUsername string
}

// telegramFingerprint — отпечаток токена: смена токена перезапускает
// поллер, а сам токен поллер не хранит.
func telegramFingerprint(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// sync приводит набор поллеров к базе: останавливает лишние и поллеры со
// сменённым токеном, запускает недостающие. Ошибка — набор не сверен: база
// не прочлась или поллеры сервера остановлены; набор тогда остаётся
// прежним до следующей сверки.
func (b *telegramBots) sync(ctx context.Context) error {
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	return b.syncLocked(ctx)
}

// drop останавливает поллер своего бота пользователя, который этот бот снял,
// и сверяет набор. Поллер снятого бота остановлен и тогда, когда сверка не
// прошла.
func (b *telegramBots) drop(ctx context.Context, userID string) error {
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	if p, ok := b.pollers[userID]; ok {
		p.stop()
		delete(b.pollers, userID)
	}
	return b.syncLocked(ctx)
}

// syncLocked — sync под syncMu.
func (b *telegramBots) syncLocked(ctx context.Context) error {
	if b.ctx == nil || b.ctx.Err() != nil {
		return errTelegramStopped
	}
	// Счёт привязок — до чтения базы: привязка, подтверждённая после него,
	// может не попасть в прочитанный набор.
	b.mu.Lock()
	links := b.installLinks
	b.mu.Unlock()
	specs, chats, err := b.desired(ctx)
	if err != nil {
		return err
	}
	b.setInstallationChats(chats, links)
	for key, p := range b.pollers {
		spec, ok := specs[key]
		if ok && telegramFingerprint(spec.token) == p.fingerprint && p.alive() {
			continue
		}
		p.stop()
		delete(b.pollers, key)
	}
	for key, spec := range specs {
		if _, ok := b.pollers[key]; ok {
			continue
		}
		client, err := telegram.NewClient(spec.token, b.client)
		if err != nil {
			// Токен в базу попадает только через getMe, и форма его проверена.
			continue
		}
		pctx, cancel := context.WithCancel(b.ctx)
		p := &telegramPoller{bots: b, userID: spec.userID, botUsername: spec.botUsername, botID: telegramBotID(spec.token),
			client: client, fingerprint: telegramFingerprint(spec.token), cancel: cancel, done: make(chan struct{})}
		b.pollers[key] = p
		go p.run(pctx)
	}
	return nil
}

// desired читает из базы боты, которым нужен поллер, и чаты бота
// установки. Отключённый пользователь поллера не получает, привязка в error
// тоже: её снимает новый токен или новый код. Не получает его и токен, чей
// поллер остановили 401 или 409 (halted), даже если ошибка не записалась:
// иначе сверка тут же снова пошла бы в отказ.
//
// Свой бот, который стал ботом установки, второго поллера не получает: у
// бота одна очередь getUpdates, и второй опрос дал бы 409 одному из них.
// Его привязке desired пишет ошибку с постоянной причиной; не записалась —
// бот всё равно опрашивает только поллер установки.
func (b *telegramBots) desired(ctx context.Context) (map[string]telegramBotSpec, map[int64]string, error) {
	users, err := b.store.ListUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	specs := map[string]telegramBotSpec{}
	chats := map[int64]string{}
	for _, u := range users {
		if u.Disabled {
			continue
		}
		link, err := b.store.GetTelegramLink(ctx, u.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if !link.HasBotToken {
			if link.State == "linked" {
				chats[link.ChatID] = u.ID
			}
			continue
		}
		if link.State == "error" {
			continue
		}
		token, err := b.store.TelegramBotToken(ctx, u.ID, "")
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if b.installID != 0 && telegramBotID(token) == b.installID {
			_, _ = b.store.RecordTelegramLinkError(ctx, u.ID, telegramReasonInstallationBot, b.install)
			continue
		}
		if b.haltedToken(u.ID, token) {
			continue
		}
		specs[u.ID] = telegramBotSpec{userID: u.ID, token: token, botUsername: link.BotUsername}
	}
	if b.opts.reconciling != nil {
		b.opts.reconciling()
	}
	b.mu.Lock()
	failed := b.installFailure != ""
	b.mu.Unlock()
	if b.install != "" && !failed {
		specs[""] = telegramBotSpec{token: b.install}
	}
	return specs, chats, nil
}

// stopAll останавливает все поллеры при остановке сервера.
func (b *telegramBots) stopAll() {
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	for key, p := range b.pollers {
		p.stop()
		delete(b.pollers, key)
	}
}

// running сообщает, работает ли поллер пользователя userID ("" — бота
// установки).
func (b *telegramBots) running(userID string) bool {
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	p, ok := b.pollers[userID]
	return ok && p.alive()
}

// installation — имя бота установки из getMe и причина его остановки.
func (b *telegramBots) installation() (username, failure string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.installBot, b.installFailure
}

func (b *telegramBots) setInstallationBot(username string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.installBot = username
}

func (b *telegramBots) failInstallation(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.installFailure = reason
}

// installationChat — пользователь, к которому чат привязан через бот
// установки, пусто — ни к кому.
func (b *telegramBots) installationChat(chatID int64) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.installChats[chatID].userID
}

// linkInstallationChat запоминает чат, который привязал поллер, с номером
// привязки.
func (b *telegramBots) linkInstallationChat(chatID int64, userID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.installLinks++
	b.installChats[chatID] = telegramInstallChat{userID: userID, link: b.installLinks}
}

// setInstallationChats заменяет чаты бота установки набором chats, который
// сверка прочла из базы после links привязок поллера. Чат, привязанный
// позже, остаётся: набор его мог не видеть.
func (b *telegramBots) setInstallationChats(chats map[int64]string, links uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	next := make(map[int64]telegramInstallChat, len(chats))
	for chatID, userID := range chats {
		next[chatID] = telegramInstallChat{userID: userID}
	}
	for chatID, c := range b.installChats {
		if c.link > links {
			next[chatID] = c
		}
	}
	b.installChats = next
}

func (b *telegramBots) offset(botID int64) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.offsets[botID]
}

func (b *telegramBots) setOffset(botID, offset int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.offsets[botID] = offset
}

// forget забывает остановленный токен пользователя, снявшего свой бот и
// привязку.
func (b *telegramBots) forget(userID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.halted, userID)
}

// halt помечает токен своего бота пользователя остановленным 401 или 409.
func (b *telegramBots) halt(userID string, fingerprint [32]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.halted[userID] = fingerprint
}

// resume снимает пометку остановки: новый токен или новый код просят
// поллер снова.
func (b *telegramBots) resume(userID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.halted, userID)
}

// haltedToken: поллер токена token пользователя остановлен 401 или 409.
// Пометку другого токена, сменённого мимо API, снимает.
func (b *telegramBots) haltedToken(userID, token string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	fingerprint, ok := b.halted[userID]
	if !ok {
		return false
	}
	if fingerprint == telegramFingerprint(token) {
		return true
	}
	delete(b.halted, userID)
	return false
}

// telegramPoller — поллер одного бота.
type telegramPoller struct {
	bots        *telegramBots
	userID      string
	botUsername string
	botID       int64
	client      *telegram.Client
	fingerprint [32]byte
	cancel      context.CancelFunc
	done        chan struct{}
	// me — getMe бота установки; его читает и пишет только цикл поллера.
	me telegramGetMe
}

// telegramGetMe — ответил ли getMe бота установки, сбоев подряд и когда
// спросить его снова.
type telegramGetMe struct {
	answered bool
	failures int
	retry    time.Time
}

func (p *telegramPoller) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *telegramPoller) stop() {
	p.cancel()
	<-p.done
	p.client.Close()
}

// run — цикл long polling. Offset в памяти идёт за каждым обработанным
// обновлением. 401 и 409 останавливают поллер с причиной; порция больше
// предела клиента (ErrTooLarge) спрашивается снова с того же offset по
// одному обновлению; прочие сбои — пауза backoff.
func (p *telegramPoller) run(ctx context.Context) {
	defer close(p.done)
	offset := p.bots.offset(p.botID)
	limit, failures := 0, 0
	for ctx.Err() == nil {
		if !p.learnInstallationBot(ctx) {
			return
		}
		updates, err := p.client.GetUpdates(ctx, telegram.Poll{Offset: offset, Limit: limit, Wait: p.bots.opts.wait})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if p.fatal(ctx, err) {
				return
			}
			if errors.Is(err, telegram.ErrTooLarge) && limit != 1 {
				limit = 1
				continue
			}
			failures++
			if !sleepContext(ctx, p.pause(err, failures)) {
				return
			}
			continue
		}
		limit, failures = 0, 0
		if len(updates) > 0 && telegram.NextOffset(offset, updates) == offset {
			// Порция ниже offset: после недели тишины Telegram выбрал id
			// заново (NextOffset), и очередь идёт с её начала.
			offset = 0
		}
		for _, u := range updates {
			if ctx.Err() != nil {
				return
			}
			p.handle(ctx, u)
			// Прерванная отправка могла дойти до агента. Следующий поллер
			// не должен снова пытаться доставить обработанное обновление.
			offset = telegram.NextOffset(offset, []telegram.Update{u})
			p.bots.setOffset(p.botID, offset)
		}
	}
}

// pause — пауза после сбоя: backoff, но не меньше retry_after отказа 429.
func (p *telegramPoller) pause(err error, failures int) time.Duration {
	d := p.bots.opts.backoff(failures)
	var refused *telegram.Error
	if errors.As(err, &refused) && refused.RetryAfter > d {
		d = refused.RetryAfter
	}
	return d
}

// learnInstallationBot узнаёт имя бота установки из getMe: его называют
// профиль и ссылка t.me. getMe, который ответил, даже именем, которое не
// годится, снова не спрашивается. Сбой опросу не мешает, кроме 401 (false —
// поллер остановлен), а getMe после него спрашивается снова не раньше паузы,
// как getUpdates после сбоя (pause).
func (p *telegramPoller) learnInstallationBot(ctx context.Context) bool {
	if p.userID != "" || p.me.answered || time.Now().Before(p.me.retry) {
		return true
	}
	me, err := p.client.GetMe(ctx)
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		if p.fatal(ctx, err) {
			return false
		}
		p.me.failures++
		p.me.retry = time.Now().Add(p.pause(err, p.me.failures))
		return true
	}
	p.me.answered = true
	// Имя идёт в ссылку t.me и в профиль: годится только имя пользователя
	// Telegram по правилу хранилища для своих ботов, иначе бот остаётся
	// безымянным, как без ответа getMe.
	if store.TelegramBotName(me.Username) {
		p.bots.setInstallationBot(me.Username)
	}
	return true
}

// fatal останавливает поллер на 401 и 409: без нового токена или без
// остановки второго поллера опрос не пойдёт. Свой бот получает пометку
// halted и ошибку привязки с причиной, бот установки — причину в памяти до
// перезапуска. Пометка ставится до записи: сверка не запустит поллер
// снова, даже если записи нет.
func (p *telegramPoller) fatal(ctx context.Context, err error) bool {
	var lead string
	switch {
	case errors.Is(err, telegram.ErrUnauthorized):
		lead = telegramReasonUnauthorized
	case errors.Is(err, telegram.ErrConflict):
		lead = telegramReasonConflict
	default:
		return false
	}
	reason := telegramClip(lead+"; "+err.Error(), store.TelegramReasonLimit)
	if p.userID == "" {
		p.bots.failInstallation(reason)
	} else {
		p.bots.halt(p.userID, p.fingerprint)
		p.failLink(ctx, lead, reason)
	}
	p.bots.nudge()
	return true
}

// failLink пишет ошибку привязки, если пользователь всё ещё держит токен
// этого поллера: новый токен, записанный, пока поллер останавливался, ошибку
// старого не получает. Причину с формой токена хранилище отвергает, и тогда
// остаётся одна постоянная часть. Прочий сбой записи остаётся без ошибки
// привязки: поллер держит пометка halted.
func (p *telegramPoller) failLink(ctx context.Context, lead, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telegramFailTimeout)
	defer cancel()
	record := func(reason string) error {
		return p.bots.store.WithTx(ctx, func(ctx context.Context) error {
			token, err := p.bots.store.TelegramBotToken(ctx, p.userID, "")
			switch {
			case errors.Is(err, store.ErrNotFound):
				// Свой бот уже снят: записывать нечего.
				return nil
			case err != nil:
				return err
			case telegramFingerprint(token) != p.fingerprint:
				return nil
			}
			_, err = p.bots.store.RecordTelegramLinkError(ctx, p.userID, reason, p.bots.install)
			return err
		})
	}
	if err := record(reason); errors.Is(err, store.ErrTelegramInvalid) {
		_ = record(lead)
	}
}

// telegramClip обрезает s до limit байт по границе символа.
func telegramClip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// handle принимает сообщения только из личного чата. Непривязанный чат
// может только привязаться через /start.
func (p *telegramPoller) handle(ctx context.Context, u telegram.Update) {
	if u.Malformed {
		return
	}
	if q := u.CallbackQuery; q != nil {
		p.handleCallback(ctx, q)
		return
	}
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot || !m.Chat.Private() || m.Chat.ID <= 0 || m.Chat.ID != m.From.ID {
		return
	}
	name, arg, command := telegramCommand(m.Text)
	chat, err := p.chatOwner(ctx, m.Chat.ID, m.From.ID)
	if err != nil {
		return
	}
	if chat.linked() {
		p.noteSender(ctx, chat, m.From)
		response := p.bots.channel.receive(ctx, chat, m)
		p.respond(ctx, chat, response)
		return
	}
	if !command || name != "start" {
		return
	}
	if busy, err := p.linkedElsewhere(ctx); err != nil || busy {
		return
	}
	if arg == "" {
		p.reply(ctx, m.Chat.ID, telegramReplyStart)
		return
	}
	p.confirm(ctx, m, arg)
}

// handleCallback подтверждает нажатие даже при недоступной или старой
// привязке; в таком случае в чат ничего не отправляется.
func (p *telegramPoller) handleCallback(ctx context.Context, q *telegram.CallbackQuery) {
	m := q.Message
	if !strings.HasPrefix(q.Data, telegramUseCallback) || m == nil || q.From.IsBot || !m.Chat.Private() || m.Chat.ID <= 0 || m.Chat.ID != q.From.ID {
		_ = p.client.AnswerCallbackQuery(ctx, q.ID, telegramStaleChoice)
		return
	}
	chat, err := p.chatOwner(ctx, m.Chat.ID, q.From.ID)
	if err != nil || !chat.linked() {
		_ = p.client.AnswerCallbackQuery(ctx, q.ID, telegramStaleChoice)
		return
	}
	response, ok := p.bots.channel.useCallback(ctx, chat, q.Data)
	if !ok {
		_ = p.client.AnswerCallbackQuery(ctx, q.ID, telegramStaleChoice)
		return
	}
	_ = p.client.AnswerCallbackQuery(ctx, q.ID, "")
	p.respond(ctx, chat, response)
}

// respond рендерит choices как inline keyboard. Проверка привязки не даёт
// отправить ответ старой сессии в заново привязанный чат.
func (p *telegramPoller) respond(ctx context.Context, chat telegramChat, response telegramResponse) {
	if response.text == "" || !chat.active(ctx, p.bots.store) {
		return
	}
	var markup telegram.ReplyMarkup
	if len(response.choices) > 0 {
		keyboard := &telegram.InlineKeyboardMarkup{}
		for _, choices := range response.choices {
			var row []telegram.InlineKeyboardButton
			for _, choice := range choices {
				row = append(row, telegram.InlineKeyboardButton{Text: choice.label, CallbackData: choice.action})
			}
			keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
		}
		markup = keyboard
	}
	text := telegramClipText(strings.ToValidUTF8(response.text, "?"), telegram.MessageLimit)
	_, _ = p.client.SendMessage(ctx, chat.chatID, text, markup)
}

// telegramCommand разбирает команду бота: "/name", "/name@bot" и аргумент
// после пробела. Текст без "/" — не команда.
func telegramCommand(text string) (name, arg string, ok bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || len(fields[0]) < 2 || fields[0][0] != '/' {
		return "", "", false
	}
	name, _, _ = strings.Cut(fields[0][1:], "@")
	return name, strings.Join(fields[1:], " "), true
}

// chatOwner находит привязку чата chatID через этот бот и проверяет её
// хранилищем: бот, чат, собеседник from, сессия и включённый пользователь.
// Чат несёт сессию привязки; без сессии он не привязан.
func (p *telegramPoller) chatOwner(ctx context.Context, chatID, from int64) (telegramChat, error) {
	chat := telegramChat{userID: p.userID, botUsername: p.botUsername, chatID: chatID}
	if chat.userID == "" {
		chat.userID = p.bots.installationChat(chatID)
		if chat.userID == "" {
			return chat, nil
		}
	}
	session, err := p.bots.store.TelegramChatSession(ctx, chat.userID, p.botUsername, chatID, from)
	chat.session = session
	return chat, err
}

// noteSender пишет в привязку имя пользователя Telegram отправителя
// обновления, пришедшего под живой привязкой chat (#294): собеседник мог
// сменить или снять его после прошлого обновления. То же имя хранилище не
// пишет; сбой записи обновлению не мешает.
func (p *telegramPoller) noteSender(ctx context.Context, chat telegramChat, from *telegram.User) {
	_ = p.bots.store.NoteTelegramUsername(ctx, chat.userID, chat.session, telegramSenderName(from))
}

// telegramSenderName — имя пользователя Telegram отправителя для привязки.
// Имя, которое хранилище не примет (store.ValidTelegramUsername), считается
// отсутствующим: привязка и обновление из-за него не срываются.
func telegramSenderName(from *telegram.User) string {
	if store.ValidTelegramUsername(from.Username) {
		return from.Username
	}
	return ""
}

// linkedElsewhere: свой бот уже привязан к другому чату. Бот установки
// служит многим пользователям, и непривязанный чат может привязаться всегда.
func (p *telegramPoller) linkedElsewhere(ctx context.Context) (bool, error) {
	if p.userID == "" {
		return false, nil
	}
	link, err := p.bots.store.GetTelegramLink(ctx, p.userID)
	if err != nil {
		return false, err
	}
	return link.State == "linked", nil
}

// confirm подтверждает код привязки хранилищем вместе с именем отправителя и
// отвечает в чат.
func (p *telegramPoller) confirm(ctx context.Context, m *telegram.Message, code string) {
	link, err := p.bots.store.ConfirmTelegramLink(ctx, store.TelegramLinkConfirmation{
		BotUsername: p.botUsername, Code: code, ChatID: m.Chat.ID, TelegramUserID: m.From.ID,
		Username: telegramSenderName(m.From), Now: p.bots.opts.now(),
	})
	text := telegramReplyLinked
	switch {
	case err == nil:
		if p.userID == "" {
			p.bots.linkInstallationChat(link.ChatID, link.UserID)
		}
		p.bots.nudge()
	case errors.Is(err, store.ErrTelegramChatTaken):
		text = telegramReplyChatTaken
	case errors.Is(err, store.ErrTelegramLinkCode), errors.Is(err, store.ErrTelegramInvalid):
		text = telegramReplyCodeRefused
	default:
		text = telegramReplyLinkFailed
	}
	p.reply(ctx, m.Chat.ID, text)
}

// reply отвечает прямо в чат; пустой ответ не отправляется.
func (p *telegramPoller) reply(ctx context.Context, chatID int64, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	text = telegramClipText(strings.ToValidUTF8(text, "?"), telegram.MessageLimit)
	_, _ = p.client.SendMessage(ctx, chatID, text, nil)
}
