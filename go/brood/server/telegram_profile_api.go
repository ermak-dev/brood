package server

// API профиля Telegram (#281, эпик #278): GET/PUT/DELETE /api/me/telegram,
// POST /api/me/telegram/link и PUT /api/me/telegram/events (#295) — только
// для самого вызывающего, за сессией и CSRF middleware; запись перепроверяет
// сессию в своей транзакции. Токен бота write-only: ни один ответ и ни один
// отказ его не несут. Отказы — фиксированные публичные причины
// (telegramRefusal), текст ошибок хранилища и клиента Bot API наружу не идёт.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

const (
	// telegramBodyLimit ограничивает тело обоих PUT: токен и полный набор
	// подписок короче 150 байт.
	telegramBodyLimit = 4 << 10
	// telegramGetMeTimeout ограничивает проверку токена в запросе PUT.
	telegramGetMeTimeout = 15 * time.Second
	// telegramSyncTimeout ограничивает сверку набора поллеров после записи.
	telegramSyncTimeout = 10 * time.Second
)

// telegramSyncContext — контекст сверки поллеров после записи. Запись уже
// зафиксирована, и уход клиента сверку не отменяет: иначе поллер снятого бота
// опрашивал бы его до следующей сверки, а новый не стартовал бы до неё.
func telegramSyncContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), telegramSyncTimeout)
}

// telegramNotReconciled — запись зафиксирована, а набор поллеров сверить не
// удалось: внутренний сбой. Причина идёт в журнал, а не в ответ, и не
// оборачивается: ErrNotFound хранилища не должен стать 404.
func telegramNotReconciled(err error) error {
	return fmt.Errorf("reconcile telegram pollers: %v", err)
}

// telegramRefusal — сознательный отказ API профиля: статус и публичная
// причина, без ошибки, которая к нему привела (как command.Refusal, #276).
type telegramRefusal struct {
	status int
	reason string
}

func (e *telegramRefusal) Error() string { return e.reason }

var (
	telegramRefuseBody           = &telegramRefusal{http.StatusBadRequest, `telegram bot request must be exactly {"token"} with a string value`}
	telegramRefuseTokenShape     = &telegramRefusal{http.StatusBadRequest, "invalid telegram bot token: want <bot id>:<secret> as BotFather issues it"}
	telegramRefuseTokenRejected  = &telegramRefusal{http.StatusUnprocessableEntity, "Telegram rejected the bot token"}
	telegramRefuseBotName        = &telegramRefusal{http.StatusUnprocessableEntity, "Telegram named the bot with an unusable username"}
	telegramRefuseUnreachable    = &telegramRefusal{http.StatusServiceUnavailable, "Telegram did not answer getMe: try again later"}
	telegramRefuseGetMeRefused   = &telegramRefusal{http.StatusServiceUnavailable, "Telegram refused getMe: try again later"}
	telegramRefuseInstallBot     = &telegramRefusal{http.StatusConflict, "this is the installation bot: users without an own bot already use it"}
	telegramRefuseBotTaken       = &telegramRefusal{http.StatusConflict, "telegram bot is already in use by another user"}
	telegramRefuseNoBot          = &telegramRefusal{http.StatusConflict, "no telegram bot: set a bot token first"}
	telegramRefuseLinked         = &telegramRefusal{http.StatusConflict, "telegram chat is already linked: unlink it first"}
	telegramRefuseSessionChanged = &telegramRefusal{http.StatusForbidden, "session authorization changed"}
	// telegramRefuseOwnIsInstallBot — свой бот, сохранённый до того, как
	// оператор сделал его ботом установки.
	telegramRefuseOwnIsInstallBot = &telegramRefusal{http.StatusConflict, telegramReasonInstallationBot}
	// telegramRefuseEventsBody и telegramRefuseEventKind — отказы записи
	// подписок (#295). Неизвестный вид назван словарём хранилища, а не
	// присланным значением: отказ не повторяет тело, в нём мог быть токен.
	telegramRefuseEventsBody = &telegramRefusal{http.StatusBadRequest, `telegram events request must be exactly {"events"} with an array of strings`}
	telegramRefuseEventKind  = &telegramRefusal{http.StatusBadRequest,
		"unknown telegram event; known events: " + strings.Join(store.TelegramEventKinds(), ", ")}
)

// writeTelegramError отвечает отказом: сознательный — его причиной в detail
// и reason, прочий — apiError, внутренний сбой без подробностей.
func writeTelegramError(w http.ResponseWriter, r *http.Request, err error) {
	var refusal *telegramRefusal
	if errors.As(err, &refusal) {
		writeJSON(w, refusal.status, struct {
			Detail string `json:"detail"`
			Reason string `json:"reason"`
		}{refusal.reason, refusal.reason})
		return
	}
	apiError(w, r, err)
}

// telegramProfileJSON — профиль Telegram пользователя. BotUsername — бот,
// через который идёт привязка: свой или, без него, бот установки; OwnBot —
// задан ли свой токен. BotReason — причина остановки бота установки.
// Reason — причина ошибки привязки из хранилища. Chat — привязанный чат,
// Username — имя пользователя Telegram собеседника из хранилища (#294),
// пусто у собеседника без имени.
type telegramProfileJSON struct {
	BotUsername       string            `json:"bot_username"`
	OwnBot            bool              `json:"own_bot"`
	BotReason         string            `json:"bot_reason,omitempty"`
	State             string            `json:"state"`
	Reason            string            `json:"reason,omitempty"`
	Events            []string          `json:"events"`
	Chat              *telegramChatJSON `json:"chat"`
	LinkCodePending   bool              `json:"link_code_pending"`
	LinkCodeExpiresAt string            `json:"link_code_expires_at,omitempty"`
}

type telegramChatJSON struct {
	TelegramUserID int64  `json:"telegram_user_id"`
	ChatID         int64  `json:"chat_id"`
	Username       string `json:"username,omitempty"`
}

// telegramLinkCodeJSON — выданный код: его показывают один раз, в базе
// лежит только хэш. URL — deep link t.me, когда имя бота известно.
type telegramLinkCodeJSON struct {
	Code        string `json:"code"`
	ExpiresAt   string `json:"expires_at"`
	BotUsername string `json:"bot_username"`
	URL         string `json:"url,omitempty"`
}

func (s *Server) mountTelegramProfile() {
	s.mux.HandleFunc("GET /api/me/telegram", s.handleTelegramProfile)
	s.mux.HandleFunc("PUT /api/me/telegram", s.handleTelegramBotPut)
	s.mux.HandleFunc("DELETE /api/me/telegram", s.handleTelegramProfileDelete)
	s.mux.HandleFunc("POST /api/me/telegram/link", s.handleTelegramLinkCode)
	s.mux.HandleFunc("PUT /api/me/telegram/events", s.handleTelegramEventsPut)
}

// handleTelegramProfile — GET: профиль; пользователь без привязки получает
// пустое состояние.
func (s *Server) handleTelegramProfile(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	s.writeTelegramProfile(w, r, user.ID)
}

func (s *Server) writeTelegramProfile(w http.ResponseWriter, r *http.Request, userID string) {
	link, err := s.store.GetTelegramLink(r.Context(), userID)
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.telegram.view(link))
}

// view строит профиль из представления хранилища и того, что поллеры знают
// о боте установки.
func (b *telegramBots) view(link store.TelegramLink) telegramProfileJSON {
	out := telegramProfileJSON{BotUsername: link.BotUsername, OwnBot: link.HasBotToken, State: link.State,
		Reason: link.Reason, Events: link.Events}
	if out.Events == nil {
		out.Events = []string{}
	}
	if !link.HasBotToken {
		out.BotUsername, out.BotReason = b.installation()
	}
	if link.State == "linked" {
		out.Chat = &telegramChatJSON{TelegramUserID: link.TelegramUserID, ChatID: link.ChatID,
			Username: link.TelegramUsername}
	}
	if link.LinkCodeExpiresAt != "" {
		expires, err := time.Parse(time.RFC3339Nano, link.LinkCodeExpiresAt)
		if err == nil && b.opts.now().Before(expires) {
			out.LinkCodePending = true
			out.LinkCodeExpiresAt = expires.UTC().Format(time.RFC3339)
		}
	}
	return out
}

// handleTelegramBotPut — PUT ровно {"token"}: getMe доказывает токен и даёт
// имя бота; имя от вызывающего не принимается. Новый токен запускает поллер
// до ответа; сверка не прошла — внутренний сбой.
func (s *Server) handleTelegramBotPut(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	token, err := readTelegramToken(w, r)
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	me, err := s.proveTelegramToken(r.Context(), token)
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	installBot, _ := s.telegram.installation()
	s.telegram.opts.writing(r)
	err = s.store.WithTx(r.Context(), func(ctx context.Context) error {
		if !secretRequestSession(ctx, s, r, user.ID) {
			return telegramRefuseSessionChanged
		}
		_, err := s.store.SetTelegramBot(ctx, user.ID, token, me.Username, installBot)
		return err
	})
	switch {
	case errors.Is(err, store.ErrTelegramBotTaken):
		err = telegramRefuseBotTaken
	case errors.Is(err, store.ErrTelegramInvalid):
		err = telegramRefuseBotName
	}
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	s.telegram.opts.written(r)
	s.telegram.resume(user.ID)
	ctx, cancel := telegramSyncContext(r)
	defer cancel()
	if err := s.telegram.sync(ctx); err != nil {
		writeTelegramError(w, r, telegramNotReconciled(err))
		return
	}
	s.writeTelegramProfile(w, r, user.ID)
}

// readTelegramToken принимает объект ровно с полем token строкой. Отказ не
// повторяет тело: в нём токен.
func readTelegramToken(w http.ResponseWriter, r *http.Request) (string, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, telegramBodyLimit))
	if err != nil {
		return "", telegramRefuseBody
	}
	fields, err := proxmoxObject(string(data))
	var token string
	if err != nil || len(fields) != 1 || !syncString(fields["token"], &token) {
		return "", telegramRefuseBody
	}
	return token, nil
}

// proveTelegramToken проверяет форму токена и зовёт getMe. Бот установки
// своим ботом не становится: его id — часть его токена.
func (s *Server) proveTelegramToken(ctx context.Context, token string) (telegram.User, error) {
	client, err := telegram.NewClient(token, s.telegram.client)
	if err != nil {
		return telegram.User{}, telegramRefuseTokenShape
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(ctx, telegramGetMeTimeout)
	defer cancel()
	me, err := client.GetMe(ctx)
	var refused *telegram.Error
	isRefusal := errors.As(err, &refused) && !refused.NotAPI
	switch {
	case err == nil:
	case errors.Is(err, telegram.ErrUnauthorized), isRefusal && refused.Code == http.StatusNotFound:
		// 401 — неверный токен, 404 — токен, по которому Bot API бота не нашёл.
		return telegram.User{}, telegramRefuseTokenRejected
	case isRefusal:
		// Прочий отказ, как 429, о токене ничего не говорит.
		return telegram.User{}, telegramRefuseGetMeRefused
	default:
		return telegram.User{}, telegramRefuseUnreachable
	}
	if s.telegram.installID != 0 && me.ID == s.telegram.installID {
		return telegram.User{}, telegramRefuseInstallBot
	}
	return me, nil
}

// handleTelegramEventsPut — PUT ровно {"events"}: полный набор подписок
// вызывающего; виды сверяет хранилище, повтор схлопывается, порядок — словаря.
// Поллеры подписки не меняют: сверки после записи нет.
func (s *Server) handleTelegramEventsPut(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	events, err := readTelegramEvents(w, r)
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	var link store.TelegramLink
	s.telegram.opts.writing(r)
	err = s.store.WithTx(r.Context(), func(ctx context.Context) error {
		if !secretRequestSession(ctx, s, r, user.ID) {
			return telegramRefuseSessionChanged
		}
		var err error
		link, err = s.store.SetTelegramEvents(ctx, user.ID, events)
		return err
	})
	if errors.Is(err, store.ErrTelegramInvalid) {
		// Причина хранилища повторяет присланный вид — наружу и в журнал идёт
		// словарь.
		err = telegramRefuseEventKind
	}
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.telegram.view(link))
}

// readTelegramEvents принимает объект ровно с полем events — массивом строк.
// Виды сверяет SetTelegramEvents по словарю хранилища.
func readTelegramEvents(w http.ResponseWriter, r *http.Request) ([]string, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, telegramBodyLimit))
	if err != nil {
		return nil, telegramRefuseEventsBody
	}
	fields, err := proxmoxObject(string(data))
	raw := fields["events"]
	var items []json.RawMessage
	if err != nil || len(fields) != 1 || len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &items) != nil {
		return nil, telegramRefuseEventsBody
	}
	events := make([]string, len(items))
	for i, item := range items {
		if !syncString(item, &events[i]) {
			return nil, telegramRefuseEventsBody
		}
	}
	return events, nil
}

// handleTelegramProfileDelete — DELETE: снимает свой бот и привязку чата
// одной транзакцией; поллер своего бота остановлен до ответа, даже когда
// сверка остального набора не прошла.
func (s *Server) handleTelegramProfileDelete(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	s.telegram.opts.writing(r)
	err := s.store.WithTx(r.Context(), func(ctx context.Context) error {
		if !secretRequestSession(ctx, s, r, user.ID) {
			return telegramRefuseSessionChanged
		}
		if _, err := s.store.ClearTelegramBot(ctx, user.ID); err != nil {
			return err
		}
		_, err := s.store.UnlinkTelegram(ctx, user.ID)
		return err
	})
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	s.telegram.opts.written(r)
	s.telegram.forget(user.ID)
	ctx, cancel := telegramSyncContext(r)
	defer cancel()
	// Обещание DELETE держит drop; несверенный остаток набора сверит
	// следующий тик.
	_ = s.telegram.drop(ctx, user.ID)
	s.writeTelegramProfile(w, r, user.ID)
}

// linkRefusal — отказ выдать код, который никто не примет: бота привязки
// нет, бот установки остановлен 401 или 409 до перезапуска сервера, или свой
// бот стал ботом установки и сверка тут же сняла бы код ошибкой привязки.
func (b *telegramBots) linkRefusal(ctx context.Context, current store.TelegramLink) error {
	if !current.HasBotToken {
		if b.install == "" {
			return telegramRefuseNoBot
		}
		if _, failure := b.installation(); failure != "" {
			return &telegramRefusal{http.StatusConflict, "the installation telegram bot is stopped: " + failure}
		}
		return nil
	}
	if b.installID == 0 {
		return nil
	}
	token, err := b.store.TelegramBotToken(ctx, current.UserID, "")
	if err != nil {
		return err
	}
	if telegramBotID(token) == b.installID {
		return telegramRefuseOwnIsInstallBot
	}
	return nil
}

// handleTelegramLinkCode — POST: новый одноразовый код привязки, заменяет
// прежний и снимает ошибку привязки; поллер бота запущен до ответа, сверка
// не прошла — внутренний сбой, и код не показывается.
func (s *Server) handleTelegramLinkCode(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	code, err := newTelegramLinkCode(s.telegram.opts.random)
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	now := s.telegram.opts.now()
	expires := now.Add(telegramLinkCodeTTL)
	var link store.TelegramLink
	s.telegram.opts.writing(r)
	err = s.store.WithTx(r.Context(), func(ctx context.Context) error {
		if !secretRequestSession(ctx, s, r, user.ID) {
			return telegramRefuseSessionChanged
		}
		current, err := s.store.GetTelegramLink(ctx, user.ID)
		if err != nil {
			return err
		}
		if err := s.telegram.linkRefusal(ctx, current); err != nil {
			return err
		}
		link, err = s.store.IssueTelegramLinkCode(ctx, user.ID, code, now, expires)
		return err
	})
	if errors.Is(err, store.ErrTelegramLinked) {
		err = telegramRefuseLinked
	}
	if err != nil {
		writeTelegramError(w, r, err)
		return
	}
	s.telegram.opts.written(r)
	s.telegram.resume(user.ID)
	ctx, cancel := telegramSyncContext(r)
	defer cancel()
	if err := s.telegram.sync(ctx); err != nil {
		writeTelegramError(w, r, telegramNotReconciled(err))
		return
	}
	bot := s.telegram.view(link).BotUsername
	out := telegramLinkCodeJSON{Code: code, ExpiresAt: expires.UTC().Format(time.RFC3339), BotUsername: bot}
	if bot != "" {
		out.URL = "https://t.me/" + bot + "?start=" + code
	}
	writeJSON(w, http.StatusCreated, out)
}
