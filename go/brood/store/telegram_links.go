package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Привязка Telegram пользователя (#279): свой бот или бот установки, личный
// чат и сессия вида telegram. Каждый метод — одна транзакция; схема
// (telegram_schema.go) держит те же правила второй линией.

// telegramEventKinds — словарь видов уведомлений в порядке показа; CHECK
// telegram_outbox.kind повторяет его.
var telegramEventKinds = []string{"request_opened", "request_answered", "answer_delivery", "pass_started", "pass_finished", "step_failed", "actor_question"}

// TelegramEventKinds returns the kinds of Telegram notifications a user can
// subscribe to, in display order.
func TelegramEventKinds() []string { return slices.Clone(telegramEventKinds) }

var (
	// ErrTelegramInvalid refuses malformed link input: an empty or malformed
	// token, bot name, code or reason, a reason carrying a bot token, a code
	// without its moments, a chat that is not private, an unknown event kind.
	ErrTelegramInvalid = errors.New("invalid telegram link")
	// ErrTelegramBotTaken refuses as an own bot a bot that another user has
	// set or the installation bot: two pollers of one bot would share its
	// getUpdates queue.
	ErrTelegramBotTaken = errors.New("telegram bot is already in use")
	// ErrTelegramChatTaken refuses a private chat, and so its Telegram user,
	// linked to another user.
	ErrTelegramChatTaken = errors.New("telegram chat is linked to another user")
	// ErrTelegramLinkCode refuses an unknown, replaced, used or expired link
	// code, a code sent to a bot other than the one the link uses, and a code
	// of a disabled user. The reasons are not told apart: the chat that sends
	// a code learns nothing about the account.
	ErrTelegramLinkCode = errors.New("telegram link code is invalid or expired")
	// ErrTelegramLinked refuses a new link code while a chat is linked.
	ErrTelegramLinked = errors.New("telegram chat is already linked: unlink it first")
)

// TelegramLink is the store view of a user's Telegram link. The bot token is
// write-only: the view only says whether the user has set one and names the
// bot. A user without an own bot uses the installation bot, if there is one.
// State is unlinked, linked or error; Reason explains an error. ChatID and
// TelegramUserID are set only while linked, LinkCodeExpiresAt only while a
// link code waits. Events are the subscribed notification kinds.
type TelegramLink struct {
	UserID            string
	HasBotToken       bool
	BotUsername       string
	ChatID            int64
	TelegramUserID    int64
	State             string
	Reason            string
	LinkCodeExpiresAt string
	Events            []string
	UpdatedAt         string
}

// TelegramLinkConfirmation is a link code a bot received in a private chat:
// ChatID equals TelegramUserID, the sender. BotUsername names the user's own
// bot that received it, in any case; empty means the installation bot. Now is
// the moment the code arrived, checked against its expiry; a zero Now is
// refused, not taken as a moment before every expiry.
type TelegramLinkConfirmation struct {
	BotUsername    string
	Code           string
	ChatID         int64
	TelegramUserID int64
	Now            time.Time
}

// telegramLinkView читает представление привязки; пользователь без строки
// привязки получает её значения по умолчанию.
const telegramLinkView = `SELECT u.id, l.bot_token IS NOT NULL, coalesce(l.bot_username, ''),
	coalesce(l.chat_id, 0), coalesce(l.telegram_user_id, 0), coalesce(l.state, 'unlinked'),
	coalesce(l.reason, ''), coalesce(l.link_code_expires_at, ''), l.events, coalesce(l.updated_at, '')
	FROM users u LEFT JOIN user_telegram_links l ON l.user_id = u.id WHERE u.id = ?`

// GetTelegramLink returns the user's Telegram link; a user who never set one
// gets the unlinked default with every event kind. Unknown user: ErrNotFound.
func (s *Store) GetTelegramLink(ctx context.Context, userID string) (TelegramLink, error) {
	var link TelegramLink
	var events sql.NullString
	err := s.conn(ctx).QueryRowContext(ctx, telegramLinkView, userID).Scan(&link.UserID, &link.HasBotToken,
		&link.BotUsername, &link.ChatID, &link.TelegramUserID, &link.State, &link.Reason,
		&link.LinkCodeExpiresAt, &events, &link.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TelegramLink{}, fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	if err != nil {
		return TelegramLink{}, err
	}
	link.Events = TelegramEventKinds()
	if events.Valid {
		link.Events = nil
		if err := json.Unmarshal([]byte(events.String), &link.Events); err != nil {
			return TelegramLink{}, fmt.Errorf("decode telegram events of user %q: %w", userID, err)
		}
	}
	return link, nil
}

// SetTelegramBot stores the user's own bot token and the bot name from getMe.
// A new token of the same bot keeps the linked chat and clears a link error;
// another bot drops the link and its session: the chat was bound through the
// previous bot. Bot names are compared ignoring case, as Telegram does, here
// and wherever the store matches a bot. installationBot is the installation
// bot's name from getMe, empty without one. That bot or a bot set by another
// user: ErrTelegramBotTaken, before anything is written. The returned view
// and every error leave the token out.
func (s *Store) SetTelegramBot(ctx context.Context, userID, token, username, installationBot string) (TelegramLink, error) {
	if !telegramWord(token, 256) || !telegramBotName(username) {
		return TelegramLink{}, fmt.Errorf("%w: a bot token and the bot username from getMe are required", ErrTelegramInvalid)
	}
	if telegramSameBot(username, installationBot) {
		return TelegramLink{}, errTelegramInstallationBot
	}
	return s.changeTelegramLink(ctx, userID, func(ctx context.Context, _ TelegramLink) error {
		var taken bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_telegram_links WHERE bot_username = ? COLLATE NOCASE AND user_id <> ?)`, username, userID).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return ErrTelegramBotTaken
		}
		return nil
	}, func(ctx context.Context, current TelegramLink) error {
		if !telegramSameBot(current.BotUsername, username) || current.State == "error" {
			if err := s.dropTelegramLink(ctx, userID, "unlinked", nil); err != nil {
				return err
			}
		}
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE user_telegram_links SET bot_token = ?, bot_username = ?, updated_at = ? WHERE user_id = ?`,
			token, username, nowRFC3339(), userID)
		if IsUniqueViolation(err) {
			return ErrTelegramBotTaken
		}
		return err
	})
}

// errTelegramInstallationBot: свой бот пользователя назван ботом установки.
var errTelegramInstallationBot = fmt.Errorf("%w: it is the installation bot", ErrTelegramBotTaken)

// ClearTelegramBot removes the user's own bot. The user falls back to the
// installation bot, so a chat linked through the own bot is unlinked and its
// session deleted.
func (s *Store) ClearTelegramBot(ctx context.Context, userID string) (TelegramLink, error) {
	return s.changeTelegramLink(ctx, userID, nil, func(ctx context.Context, current TelegramLink) error {
		if !current.HasBotToken {
			return nil
		}
		if err := s.dropTelegramLink(ctx, userID, "unlinked", nil); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE user_telegram_links SET bot_token = NULL, bot_username = NULL, updated_at = ? WHERE user_id = ?`,
			nowRFC3339(), userID)
		return err
	})
}

// TelegramBotToken returns the bot token the server's Bot API client uses
// for the user: the user's own token, else the installation token fallback
// from .env. Neither, or an unknown user: ErrNotFound. Only the server reads
// the token; no API response carries it.
func (s *Store) TelegramBotToken(ctx context.Context, userID, fallback string) (string, error) {
	var token sql.NullString
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT l.bot_token FROM users u LEFT JOIN user_telegram_links l ON l.user_id = u.id WHERE u.id = ?`, userID).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	switch {
	case err != nil:
		return "", err
	case token.Valid:
		return token.String, nil
	case fallback != "":
		return fallback, nil
	}
	return "", fmt.Errorf("%w: telegram bot token", ErrNotFound)
}

// IssueTelegramLinkCode stores the hash of a one-time link code issued at now
// and valid until expiresAt, replacing an earlier code and clearing a link
// error. The user sends the code to the bot the link uses. A code shorter
// than 16 bytes, a zero now, or an expiry not after it or more than an hour
// after it: ErrTelegramInvalid. A code names one user: the same code still
// live for another user is refused with ErrTelegramInvalid, an expired one is
// taken from that user. A linked user: ErrTelegramLinked.
func (s *Store) IssueTelegramLinkCode(ctx context.Context, userID, code string, now, expiresAt time.Time) (TelegramLink, error) {
	if !telegramWord(code, 256) || len(code) < telegramLinkCodeMin || now.IsZero() ||
		!expiresAt.After(now) || expiresAt.After(now.Add(telegramLinkCodeMaxTTL)) {
		return TelegramLink{}, fmt.Errorf("%w: a link code of %d to 256 bytes, the moment it is issued and a later expiry at most an hour after it are required",
			ErrTelegramInvalid, telegramLinkCodeMin)
	}
	hash := telegramCodeHash(code)
	return s.changeTelegramLink(ctx, userID, func(ctx context.Context, current TelegramLink) error {
		if current.State == "linked" {
			return ErrTelegramLinked
		}
		return s.checkTelegramCodeFree(ctx, userID, hash, now)
	}, func(ctx context.Context, _ TelegramLink) error {
		if err := s.releaseTelegramCode(ctx, userID, hash); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE user_telegram_links SET state = 'unlinked', reason = NULL, link_code_hash = ?, link_code_expires_at = ?, updated_at = ? WHERE user_id = ?`,
			hash, expiresAt.UTC().Format(time.RFC3339Nano), nowRFC3339(), userID)
		if IsUniqueViolation(err) {
			return errTelegramCodeIssued
		}
		return err
	})
}

// Код привязки — пароль на время: короткий или долгоживущий код подобрал бы
// перебором любой, кто пишет боту. telegramLinkCodeMin — наименьшая длина
// выдаваемого кода в байтах, telegramLinkCodeMaxTTL — наибольший срок от выдачи.
const (
	telegramLinkCodeMin    = 16
	telegramLinkCodeMaxTTL = time.Hour
)

// errTelegramCodeIssued: код уже выдан другому пользователю и действует.
var errTelegramCodeIssued = fmt.Errorf("%w: the link code is already issued", ErrTelegramInvalid)

// checkTelegramCodeFree отвергает код, который у другого пользователя ещё
// действует в момент now. Истёкший код ничего не привязывает и значение
// UNIQUE не держит: его снимет releaseTelegramCode.
func (s *Store) checkTelegramCodeFree(ctx context.Context, userID, hash string, now time.Time) error {
	var expires string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT link_code_expires_at FROM user_telegram_links WHERE link_code_hash = ? AND user_id <> ?`,
		hash, userID).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	live, err := telegramCodeLive(expires, now)
	if err != nil {
		return err
	}
	if live {
		return errTelegramCodeIssued
	}
	return nil
}

// releaseTelegramCode снимает тот же код с другого пользователя: после
// checkTelegramCodeFree он там только истёкший.
func (s *Store) releaseTelegramCode(ctx context.Context, userID, hash string) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE user_telegram_links SET link_code_hash = NULL, link_code_expires_at = NULL, updated_at = ? WHERE link_code_hash = ? AND user_id <> ?`,
		nowRFC3339(), hash, userID)
	return err
}

// telegramCodeLive: код со сроком expires действует в момент now; срок —
// первый момент, когда код уже не принимается.
func telegramCodeLive(expires string, now time.Time) (bool, error) {
	deadline, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return false, fmt.Errorf("parse telegram link code expiry: %w", err)
	}
	return now.Before(deadline), nil
}

// ConfirmTelegramLink links the private chat that sent a valid link code
// through the bot the link uses, and opens the link's telegram session in the
// same transaction. The code is single-use. A zero Now or a chat that is not
// private (its id is not the positive id of the sender): ErrTelegramInvalid.
// A chat linked to another user: ErrTelegramChatTaken; any code refusal:
// ErrTelegramLinkCode. A refusal writes nothing, even inside a caller's
// transaction that goes on after it.
func (s *Store) ConfirmTelegramLink(ctx context.Context, in TelegramLinkConfirmation) (TelegramLink, error) {
	if !telegramWord(in.Code, 256) || in.Now.IsZero() || in.ChatID <= 0 || in.ChatID != in.TelegramUserID {
		return TelegramLink{}, fmt.Errorf("%w: a link code, the moment it arrived and a private chat whose id is its sender's are required", ErrTelegramInvalid)
	}
	var link TelegramLink
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var userID, expires string
		var bot sql.NullString
		err := s.conn(ctx).QueryRowContext(ctx, `SELECT user_id, bot_username, link_code_expires_at FROM user_telegram_links WHERE link_code_hash = ?`,
			telegramCodeHash(in.Code)).Scan(&userID, &bot, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTelegramLinkCode
		}
		if err != nil {
			return err
		}
		// Код принимает только бот привязки: свой бот пользователя, а без
		// него — бот установки. Иначе чат чужого бота стал бы чатом
		// пользователя. Имя бота сравнивается без регистра.
		live, err := telegramCodeLive(expires, in.Now)
		if err != nil {
			return err
		}
		if !telegramSameBot(bot.String, in.BotUsername) || !live {
			return ErrTelegramLinkCode
		}
		// Занятый чат отвергается до сессии: вложенный WithTx не откатывает
		// вставленную сессию, если вызывающий обработал отказ.
		var taken bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_telegram_links WHERE chat_id = ? AND user_id <> ?)`, in.ChatID, userID).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return ErrTelegramChatTaken
		}
		session, err := s.openTelegramSession(ctx, userID)
		if err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE user_telegram_links SET state = 'linked', reason = NULL, chat_id = ?, telegram_user_id = ?, session_hash = ?, link_code_hash = NULL, link_code_expires_at = NULL, updated_at = ? WHERE user_id = ?`,
			in.ChatID, in.TelegramUserID, session, nowRFC3339(), userID)
		if IsUniqueViolation(err) {
			return ErrTelegramChatTaken
		}
		if err != nil {
			return err
		}
		link, err = s.GetTelegramLink(ctx, userID)
		return err
	})
	if err != nil {
		return TelegramLink{}, err
	}
	return link, nil
}

// openTelegramSession вставляет сессию вида telegram без срока: она живёт,
// пока жива привязка. Ключ — не хэш токена, а случайный идентификатор с
// префиксом: токена у сессии нет, и ни один SHA-256 cookie с ним не совпадёт.
// Отключённому пользователю сессия не открывается, как CreateSession.
func (s *Store) openTelegramSession(ctx context.Context, userID string) (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("read telegram session key: %w", err)
	}
	key := "telegram:" + hex.EncodeToString(random[:])
	result, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at, data, kind)
		SELECT ?, ?, ?, NULL, NULL, 'telegram' WHERE NOT EXISTS (SELECT 1 FROM users WHERE id = ? AND disabled = 1)`,
		key, userID, nowRFC3339(), userID)
	if err != nil {
		return "", fmt.Errorf("create telegram session: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return "", err
	} else if n == 0 {
		return "", ErrTelegramLinkCode
	}
	return key, nil
}

// UnlinkTelegram unlinks the user's chat and deletes the link's telegram
// session in one transaction, so no answer from the chat passes afterwards.
// It also clears a waiting code and a link error.
func (s *Store) UnlinkTelegram(ctx context.Context, userID string) (TelegramLink, error) {
	return s.changeTelegramLink(ctx, userID, nil, func(ctx context.Context, _ TelegramLink) error {
		return s.dropTelegramLink(ctx, userID, "unlinked", nil)
	})
}

// RecordTelegramLinkError marks the link failed with a public reason (a token
// Telegram rejected, a chat that blocked the bot) and deletes its session: a
// failed link acts for no one until the user links the chat again. The
// reason reaches the link view, so it is refused with ErrTelegramInvalid when
// it carries a bot token: the user's own token or installationToken (empty
// without one), whole or its secret part as a word, a token in a Bot API URL
// (".../bot<id>:<secret>/..." in any case), or any text shaped like a Bot API
// token, such as a previous own token. The checks also read the text with up
// to three layers of percent-encoding removed, each valid %XX escape on its
// own, so a stray '%' elsewhere does not hide an encoded token. The caller
// redacts an HTTP error before recording it.
func (s *Store) RecordTelegramLinkError(ctx context.Context, userID, reason, installationToken string) (TelegramLink, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 512 {
		return TelegramLink{}, fmt.Errorf("%w: a link error needs a reason of at most 512 bytes", ErrTelegramInvalid)
	}
	return s.changeTelegramLink(ctx, userID, func(ctx context.Context, _ TelegramLink) error {
		var own string
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT coalesce((SELECT bot_token FROM user_telegram_links WHERE user_id = ?), '')`, userID).Scan(&own); err != nil {
			return err
		}
		if telegramReasonCarriesToken(reason, own, installationToken) {
			return errTelegramReasonToken
		}
		return nil
	}, func(ctx context.Context, _ TelegramLink) error {
		return s.dropTelegramLink(ctx, userID, "error", reason)
	})
}

// telegramBotAPIPath: токен в адресе Bot API — путь /bot<id>:<secret>/ в
// любом регистре. Так его несёт текст ошибки net/http.
var telegramBotAPIPath = regexp.MustCompile(`(?i)/bot[0-9]+:`)

// telegramTokenShape: текст формы токена Bot API — id бота и секрет из
// знаков алфавита токена, не короче 30 (у Bot API их 35). Так ловится токен,
// которого хранилище не знает: прежний токен своего бота или токен чужого.
var telegramTokenShape = regexp.MustCompile(`[0-9]{5,}:[A-Za-z0-9_-]{30,}`)

// telegramReasonDecodings — сколько слоёв percent-кодирования снимает
// проверка причины: адрес с токеном, закодированный ещё раз, несёт %253A;
// третий слой — запас.
const telegramReasonDecodings = 3

// telegramReasonCarriesToken: причина несёт токен бота — адрес Bot API,
// текст формы токена или один из known, целиком или его секрет (часть после
// двоеточия) отдельным словом. Проверяется сам текст и каждый из слоёв,
// которые снимает telegramPercentDecode: снятое кодирование может и открыть
// токен, и приклеить к секрету букву, которая скрыла бы его в следующем слое.
func telegramReasonCarriesToken(reason string, known ...string) bool {
	text := reason
	for decodings := 0; ; decodings++ {
		if telegramTextCarriesToken(text, known) {
			return true
		}
		if decodings == telegramReasonDecodings {
			return false
		}
		decoded := telegramPercentDecode(text)
		if decoded == text {
			return false
		}
		text = decoded
	}
}

// telegramTextCarriesToken: один слой текста причины несёт токен бота.
func telegramTextCarriesToken(text string, known []string) bool {
	if telegramBotAPIPath.MatchString(text) || telegramTokenShape.MatchString(text) {
		return true
	}
	for _, token := range known {
		if token == "" {
			continue
		}
		if strings.Contains(text, token) {
			return true
		}
		if _, secret, ok := strings.Cut(token, ":"); ok && secret != "" && containsTelegramWord(text, secret) {
			return true
		}
	}
	return false
}

// telegramPercentDecode снимает один слой percent-кодирования: каждый
// правильный %XX, в любом регистре цифр, становится своим байтом, а одинокий
// или неправильный '%' остаётся как есть. url.PathUnescape отказывает всей
// строке из-за одного «100%», и токен рядом остался бы закодированным.
func telegramPercentDecode(text string) string {
	if !strings.Contains(text, "%") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		if text[i] == '%' && i+2 < len(text) {
			if b, err := hex.DecodeString(text[i+1 : i+3]); err == nil {
				out.WriteByte(b[0])
				i += 2
				continue
			}
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

// containsTelegramWord: word стоит в text отдельным словом алфавита токена,
// без букв, цифр, '_' и '-' по краям. Иначе короткий секрет отвергал бы
// любой текст со своей буквой.
func containsTelegramWord(text, word string) bool {
	for start := 0; start < len(text); {
		i := strings.Index(text[start:], word)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(word)
		if (i == 0 || !telegramTokenByte(text[i-1])) && (end == len(text) || !telegramTokenByte(text[end])) {
			return true
		}
		start = i + 1
	}
	return false
}

// telegramTokenByte: байт алфавита токена Bot API.
func telegramTokenByte(b byte) bool {
	return b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// errTelegramReasonToken: причина ошибки несёт токен бота; сам токен отказ
// не повторяет.
var errTelegramReasonToken = fmt.Errorf("%w: a link error reason must not carry a bot token", ErrTelegramInvalid)

// SetTelegramEvents replaces the user's subscriptions with the given event
// kinds, deduplicated in vocabulary order. An unknown kind is refused with the
// list of known ones.
func (s *Store) SetTelegramEvents(ctx context.Context, userID string, events []string) (TelegramLink, error) {
	for _, event := range events {
		if !slices.Contains(telegramEventKinds, event) {
			return TelegramLink{}, fmt.Errorf("%w: unknown telegram event %q; known events: %s",
				ErrTelegramInvalid, event, strings.Join(telegramEventKinds, ", "))
		}
	}
	subscribed := []string{}
	for _, kind := range telegramEventKinds {
		if slices.Contains(events, kind) {
			subscribed = append(subscribed, kind)
		}
	}
	encoded, err := json.Marshal(subscribed)
	if err != nil {
		return TelegramLink{}, err
	}
	return s.changeTelegramLink(ctx, userID, nil, func(ctx context.Context, _ TelegramLink) error {
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE user_telegram_links SET events = ?, updated_at = ? WHERE user_id = ?`,
			string(encoded), nowRFC3339(), userID)
		return err
	})
}

// TelegramSessionActive reports whether the user's link is live for an update
// that botUsername received from chatID, sent by telegramUserID: the link was
// made through that bot (botUsername is the user's own bot in any case, empty
// for the installation bot) and is linked to that chat and sender, its
// telegram session exists and the user is enabled. Called with the
// transaction context of a write (the authorize callback of
// AnswerSwarmRequest), it sees an unlink or a revocation committed before the
// write.
func (s *Store) TelegramSessionActive(ctx context.Context, userID, botUsername string, chatID, telegramUserID int64) (bool, error) {
	var live bool
	// Результат coalesce не несёт COLLATE колонки, поэтому NOCASE назван явно.
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_telegram_links l
		JOIN sessions s ON s.token_hash = l.session_hash JOIN users u ON u.id = l.user_id
		WHERE l.user_id = ? AND coalesce(l.bot_username, '') = ? COLLATE NOCASE AND l.state = 'linked'
			AND l.chat_id = ? AND l.telegram_user_id = ?
			AND s.kind = 'telegram' AND s.user_id = l.user_id AND u.disabled = 0)`,
		userID, botUsername, chatID, telegramUserID).Scan(&live)
	return live, err
}

// changeTelegramLink выполняет правку привязки пользователя в одной
// транзакции и возвращает итоговое представление. Обоим шагам дано
// представление до правки. check отвергает правку до любой записи, и строки
// по умолчанию тоже: вложенный WithTx не открывает точку сохранения, и
// вызывающий, обработавший отказ, иначе зафиксировал бы частичную запись.
// check может быть nil. change пишет; строка привязки к нему уже заведена.
// Неизвестный пользователь: ErrNotFound.
func (s *Store) changeTelegramLink(ctx context.Context, userID string, check, change func(ctx context.Context, current TelegramLink) error) (TelegramLink, error) {
	var link TelegramLink
	err := s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.GetTelegramLink(ctx, userID)
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(ctx, current); err != nil {
				return err
			}
		}
		if err := s.ensureTelegramLink(ctx, userID); err != nil {
			return err
		}
		if err := change(ctx, current); err != nil {
			return err
		}
		link, err = s.GetTelegramLink(ctx, userID)
		return err
	})
	if err != nil {
		return TelegramLink{}, err
	}
	return link, nil
}

// ensureTelegramLink заводит строку привязки пользователя, которого уже нашёл
// changeTelegramLink: без бота, без чата, подписанную на все виды
// уведомлений.
func (s *Store) ensureTelegramLink(ctx context.Context, userID string) error {
	events, err := json.Marshal(telegramEventKinds)
	if err != nil {
		return err
	}
	_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO user_telegram_links (user_id, state, events, updated_at) VALUES (?, 'unlinked', ?, ?) ON CONFLICT (user_id) DO NOTHING`,
		userID, string(events), nowRFC3339())
	return err
}

// dropTelegramLink снимает привязку: state unlinked или error с причиной,
// без чата, кода и сессии. Ссылка на сессию снимается до её удаления, поэтому
// sessions_telegram_revoke строку не находит и причину не подменяет.
func (s *Store) dropTelegramLink(ctx context.Context, userID, state string, reason any) error {
	var session sql.NullString
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT session_hash FROM user_telegram_links WHERE user_id = ?`, userID).Scan(&session); err != nil {
		return err
	}
	if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE user_telegram_links SET state = ?, reason = ?, chat_id = NULL, telegram_user_id = NULL, session_hash = NULL, link_code_hash = NULL, link_code_expires_at = NULL, updated_at = ? WHERE user_id = ?`,
		state, reason, nowRFC3339(), userID); err != nil {
		return err
	}
	if !session.Valid {
		return nil
	}
	_, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, session.String)
	return err
}

// telegramCodeHash — SHA-256 кода привязки в hex: то, что лежит в базе.
func telegramCodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// telegramWord: непустое значение не длиннее limit байт без пробелов и
// управляющих символов: токен бота и код привязки.
func telegramWord(value string, limit int) bool {
	return value != "" && len(value) <= limit &&
		strings.IndexFunc(value, func(r rune) bool { return r <= ' ' || r == 0x7f }) < 0
}

// telegramBotName: имя бота из getMe — латиница, цифры и подчёркивание, без @.
func telegramBotName(name string) bool {
	return name != "" && len(name) <= 64 && strings.IndexFunc(name, func(r rune) bool {
		return !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0
}

// telegramSameBot: имена называют одного бота. Telegram не различает регистр
// имени, и сравнение повторяет COLLATE NOCASE колонки bot_username: без
// регистра только латиница, остальные байты как есть. strings.EqualFold
// сверх того сводил бы символы Unicode, и Go с UNIQUE схемы разошлись бы.
func telegramSameBot(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if asciiLower(a[i]) != asciiLower(b[i]) {
			return false
		}
	}
	return true
}

// asciiLower: строчная латинская буква вместо заглавной, прочие байты как есть.
func asciiLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}
