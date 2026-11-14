package store

import (
	"context"
	"database/sql"
)

// Хранилище Telegram (#279, эпик #278): привязка пользователя к своему боту
// и личному чату, исходящая очередь уведомлений и серверная сессия вида
// telegram. CHECK и триггеры держат правила и при записи мимо кода:
// транзакционная проверка в Go — первая линия, схема — вторая.
// Регистрация — schemaModules ("telegram") и verifyTelegramSchema; шаги
// 28→29, 29→30 и 30→31 — schemaMigrations (migration.go). Шаг 28→29 создаёт
// таблицы и триггер отзыва текстом версии 29 (telegramSchemaV29): живой DDL с
// версии 30 несёт рой по умолчанию, с версии 31 — имя собеседника и триггер,
// который его снимает, и следующие шаги добавляют их сами
// (TestEachMigrationStepYieldsTheNextFrozenVersion).

// sessionKindColumn — вид серверной сессии (#279): web — вход браузера по
// cookie, telegram — право бота действовать от имени пользователя в
// привязанном чате. Колонка с DEFAULT добавляется ALTER без перестройки
// таблицы; schema.sql повторяет текст, который ALTER оставляет в
// sqlite_master. CHECK проверяется и на строках прежней базы.
const sessionKindColumn = `ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'web' CHECK (kind IN ('web', 'telegram'))`

// userTelegramLinksSchema — одна строка на пользователя.
//   - bot_token — токен своего бота, write-only: представление хранилища его
//     не несёт, читает только клиент Bot API сервера (TelegramBotToken).
//     NULL — бот установки из .env, если он задан; свой токен приоритетнее.
//     bot_username приходит из getMe вместе с токеном; бот принадлежит одному
//     пользователю, иначе два поллера делили бы одну очередь getUpdates.
//     Telegram не различает регистр имени бота, поэтому UNIQUE и сравнения
//     идут COLLATE NOCASE (telegramSameBot в Go): тот же токен под именем в
//     другом регистре — тот же бот.
//   - chat_id и telegram_user_id — личный чат и его собеседник. В личном чате
//     Telegram id чата — id собеседника (CHECK), группа (id < 0) не
//     привязывается. Чат один у одного пользователя brood (UNIQUE), а с ним и
//     пользователь Telegram, поэтому обновление из чата бота установки
//     адресуется однозначно.
//   - link_code_hash — SHA-256 одноразового кода привязки, link_code_expires_at
//     — его срок; сам код в базе не хранится, как токен сессии.
//   - session_hash — сессия вида telegram, которую открыла привязка: есть
//     ровно у linked. Отвязка и ошибка удаляют её, отзыв сессий пользователя
//     снимает привязку (sessionTelegramRevoke).
//   - state: unlinked, linked или error; reason — причина ошибки для
//     пользователя, есть ровно у error.
//   - events — JSON-список видов уведомлений (TelegramEventKinds), на которые
//     пользователь подписан; словарь сверяет Go.
//   - default_swarm_id — рой по умолчанию команд чата (/use, #284). Право
//     читать рой проверяет запись (SetTelegramSwarm) и каждая команда, которая
//     рой берёт, а не схема: право меняется без записи в привязку. Удаление
//     строки роя снимает значение (ON DELETE SET NULL). Колонку добавляет
//     ALTER шага 29→30 (telegramDefaultSwarmColumn), поэтому текст повторяет
//     то, что ALTER оставляет в sqlite_master: колонка сразу за последней
//     колонкой, перед ограничениями таблицы.
//   - telegram_username — имя пользователя Telegram собеседника (#294): из
//     подтверждения кода, затем из каждого обновления живой привязки, NULL у
//     собеседника без имени. Оно есть только у linked: отвязка, ошибка, смена
//     бота и отзыв сессии его снимают. Колонку добавляет ALTER шага 30→31
//     (telegramUsernameStep) за колонкой роя по умолчанию.
const userTelegramLinksSchema = `CREATE TABLE user_telegram_links (
    user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    bot_token TEXT CHECK (length(bot_token) > 0),
    bot_username TEXT UNIQUE COLLATE NOCASE CHECK (length(bot_username) > 0),
    chat_id INTEGER UNIQUE CHECK (chat_id > 0),
    telegram_user_id INTEGER,
    link_code_hash TEXT UNIQUE,
    link_code_expires_at TEXT,
    session_hash TEXT UNIQUE REFERENCES sessions(token_hash),
    state TEXT NOT NULL CHECK (state IN ('unlinked', 'linked', 'error')),
    reason TEXT CHECK (length(reason) > 0),
    events TEXT NOT NULL CHECK (json_valid(events) AND json_type(events) = 'array'),
    updated_at TEXT NOT NULL, ` + telegramDefaultSwarmDef + `, ` + telegramUsernameDef + `,
    CHECK ((bot_token IS NULL) = (bot_username IS NULL)),
    CHECK ((link_code_hash IS NULL) = (link_code_expires_at IS NULL)),
    CHECK ((state = 'linked') = (session_hash IS NOT NULL)),
    CHECK ((state = 'linked') = (chat_id IS NOT NULL)),
    CHECK ((state = 'linked') = (telegram_user_id IS NOT NULL)),
    CHECK (chat_id = telegram_user_id),
    CHECK (state <> 'linked' OR link_code_hash IS NULL),
    CHECK ((state = 'error') = (reason IS NOT NULL))
)`

// telegramDefaultSwarmDef — определение колонки роя по умолчанию: его текст
// ALTER шага 29→30 вставляет в CREATE TABLE как есть.
const telegramDefaultSwarmDef = `default_swarm_id TEXT REFERENCES swarms(id) ON DELETE SET NULL`

// telegramDefaultSwarmColumn — шаг 29→30: колонка с NULL по умолчанию
// добавляется ALTER без перестройки таблицы, на которую ссылается триггер
// sessions_telegram_revoke; прежние привязки остаются без роя по умолчанию.
const telegramDefaultSwarmColumn = `ALTER TABLE user_telegram_links ADD COLUMN ` + telegramDefaultSwarmDef

// telegramUsernameDef — определение колонки имени собеседника: имя Telegram
// (ValidTelegramUsername: 1–32 латинских букв, цифр и '_', без @) и только у
// живой привязки. Его текст ALTER шага 30→31 вставляет в CREATE TABLE как
// есть.
const telegramUsernameDef = `telegram_username TEXT CHECK (telegram_username IS NULL OR (state = 'linked' AND length(telegram_username) BETWEEN 1 AND 32 AND telegram_username NOT GLOB '*[^A-Za-z0-9_]*'))`

// telegramUsernameStep — шаг 30→31: колонка с NULL по умолчанию
// добавляется ALTER без перестройки таблицы, CHECK проверяется и на прежних
// строках; прежние привязки остаются без имени до следующего сообщения чата.
// Триггер отзыва шаг заменяет текущим, который снимает и имя: прежний оставил
// бы его у error, и CHECK отверг бы удаление сессии.
const telegramUsernameStep = `ALTER TABLE user_telegram_links ADD COLUMN ` + telegramUsernameDef +
	`;DROP TRIGGER sessions_telegram_revoke;` + sessionTelegramRevoke

// userTelegramLinksSchemaV29 — замороженный текст таблицы привязок версии 29:
// им шаг 28→29 создаёт таблицу, а колонку роя по умолчанию добавляет шаг
// 29→30.
const userTelegramLinksSchemaV29 = `CREATE TABLE user_telegram_links (
    user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    bot_token TEXT CHECK (length(bot_token) > 0),
    bot_username TEXT UNIQUE COLLATE NOCASE CHECK (length(bot_username) > 0),
    chat_id INTEGER UNIQUE CHECK (chat_id > 0),
    telegram_user_id INTEGER,
    link_code_hash TEXT UNIQUE,
    link_code_expires_at TEXT,
    session_hash TEXT UNIQUE REFERENCES sessions(token_hash),
    state TEXT NOT NULL CHECK (state IN ('unlinked', 'linked', 'error')),
    reason TEXT CHECK (length(reason) > 0),
    events TEXT NOT NULL CHECK (json_valid(events) AND json_type(events) = 'array'),
    updated_at TEXT NOT NULL,
    CHECK ((bot_token IS NULL) = (bot_username IS NULL)),
    CHECK ((link_code_hash IS NULL) = (link_code_expires_at IS NULL)),
    CHECK ((state = 'linked') = (session_hash IS NOT NULL)),
    CHECK ((state = 'linked') = (chat_id IS NOT NULL)),
    CHECK ((state = 'linked') = (telegram_user_id IS NOT NULL)),
    CHECK (chat_id = telegram_user_id),
    CHECK (state <> 'linked' OR link_code_hash IS NULL),
    CHECK ((state = 'error') = (reason IS NOT NULL))
)`

// telegramSessionRevoked — причина ошибки привязки, которую пишет отзыв её
// сессии.
const telegramSessionRevoked = "telegram session was revoked: link the chat again"

// sessionTelegramRevoke: удаление сессии, на которую ссылается привязка,
// снимает привязку с причиной. Так блокировка, смена роли и смена пароля
// (DeleteSessionsOfUser) отзывают и право бота, а внешний ключ привязки не
// мешает удалению. Триггер на любом виде сессии: привязка, которую запись
// мимо хранилища направила на веб-сессию, иначе запретила бы её удаление.
// Отвязка сначала снимает ссылку сама, и триггер её не находит. Имя
// собеседника снимается вместе с чатом (#294).
const sessionTelegramRevoke = `CREATE TRIGGER sessions_telegram_revoke BEFORE DELETE ON sessions BEGIN UPDATE user_telegram_links SET state = 'error', reason = '` + telegramSessionRevoked + `', chat_id = NULL, telegram_user_id = NULL, telegram_username = NULL, session_hash = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE session_hash = OLD.token_hash; END`

// sessionTelegramRevokeV29 — замороженный текст триггера отзыва версий 29 и
// 30, без имени собеседника: им шаг 28→29 создаёт триггер, а шаг 30→31
// заменяет его текущим.
const sessionTelegramRevokeV29 = `CREATE TRIGGER sessions_telegram_revoke BEFORE DELETE ON sessions BEGIN UPDATE user_telegram_links SET state = 'error', reason = '` + telegramSessionRevoked + `', chat_id = NULL, telegram_user_id = NULL, session_hash = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE session_hash = OLD.token_hash; END`

// telegramOutboxSchema — исходящие уведомления пользователю. Строку пишет
// та же транзакция, что доменное изменение; отправка идёт после коммита, по
// порядку id на пользователя. sendMessage не идемпотентен, поэтому исход
// попытки хранится, а не выводится. attempts — начатые попытки,
// settled_attempts — попытки с записанным исходом. Отправитель отмечает
// попытку до sendMessage (attempts на единицу больше settled_attempts —
// попытка в полёте) и записывает исход после него вместе с
// settled_attempts = attempts. Обрыв между ними оставляет попытку в полёте;
// следующей попытки она не получает, а восстановление при запуске
// (RecoverTelegramOutbox) записывает ей исход unconfirmed. Состояние — исход
// последней записанной попытки:
//   - pending — в очереди: впервые или явным повтором failed;
//   - sent — Telegram принял сообщение, telegram_message_id его номер;
//   - failed — сообщение заведомо не ушло, явный повтор возвращает его в
//     pending;
//   - unconfirmed — запрос ушёл, ответа нет: сообщение могло дойти, повтор
//     только явный и без очереди, его попытка тоже в полёте до исхода.
//
// kind — словарь видов уведомлений, ref — ссылка на доменный объект,
// payload — данные сообщения. id — rowid в порядке вставки; AUTOINCREMENT
// не отдаёт номер удалённого уведомления (telegramOutboxNoDelete) новому, а
// явный номер держит telegramOutboxFreshID.
const telegramOutboxSchema = `CREATE TABLE telegram_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('request_opened', 'request_answered', 'answer_delivery', 'pass_started', 'pass_finished', 'step_failed', 'actor_question')),
    ref TEXT NOT NULL CHECK (length(ref) > 0),
    payload TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'sent', 'failed', 'unconfirmed')),
    attempts INTEGER NOT NULL CHECK (attempts >= 0),
    settled_attempts INTEGER NOT NULL CHECK (settled_attempts >= 0),
    telegram_message_id INTEGER,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((state = 'sent') = (telegram_message_id IS NOT NULL)),
    CHECK (attempts - settled_attempts IN (0, 1)),
    CHECK (state = 'pending' OR settled_attempts > 0),
    CHECK (attempts = settled_attempts OR state IN ('pending', 'unconfirmed'))
)`

// telegramOutboxPending: отправитель читает очередь пользователя по
// порядку id. Попытка в полёте остаётся головой очереди: следующее
// уведомление не обгоняет её, а повторить её ограждение не даст.
const telegramOutboxPending = `CREATE INDEX telegram_outbox_pending ON telegram_outbox(user_id, id) WHERE state = 'pending'`

// telegramOutboxUser: каскад удаления пользователя ищет его уведомления по
// user_id без условия очереди, и частичный индекс ему не годится.
const telegramOutboxUser = `CREATE INDEX telegram_outbox_user ON telegram_outbox(user_id)`

// telegramOutboxInsert: уведомление входит в очередь pending, без попыток и
// сообщения; попытку отмечает и закрывает только отправитель. Иначе вставка
// записала бы отправленным то, что не уходило, или попытку в полёте, которой
// восстановление сразу записало бы неизвестный исход.
const telegramOutboxInsert = `CREATE TRIGGER telegram_outbox_insert BEFORE INSERT ON telegram_outbox WHEN NEW.state IS NOT 'pending' OR NEW.attempts IS NOT 0 OR NEW.settled_attempts IS NOT 0 OR NEW.telegram_message_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'telegram notification is queued pending'); END`

// telegramOutboxFence: уведомление (адресат, вид, ссылка, данные, момент
// создания) не меняется, попытки не убывают, а отправленное не
// переписывается ничем: повтор дал бы в чате второе сообщение.
const telegramOutboxFence = `CREATE TRIGGER telegram_outbox_fence BEFORE UPDATE ON telegram_outbox WHEN NEW.id IS NOT OLD.id OR NEW.user_id IS NOT OLD.user_id OR NEW.kind IS NOT OLD.kind OR NEW.ref IS NOT OLD.ref OR NEW.payload IS NOT OLD.payload OR NEW.created_at IS NOT OLD.created_at OR NEW.attempts < OLD.attempts OR NEW.settled_attempts < OLD.settled_attempts OR (OLD.state = 'sent' AND (NEW.state IS NOT OLD.state OR NEW.attempts IS NOT OLD.attempts OR NEW.telegram_message_id IS NOT OLD.telegram_message_id OR NEW.updated_at IS NOT OLD.updated_at)) BEGIN SELECT RAISE(ABORT, 'telegram notification is immutable'); END`

// telegramOutboxAttempts: попытка отмечается до отправки и закрывается
// исходом. Новая попытка начинается только у строки без попытки в полёте и
// одна, без исхода в той же записи: иначе sendMessage прерванной попытки,
// который мог дойти, повторился бы. Исход попытки — sent, failed или
// unconfirmed, но не pending: возврат в очередь стёр бы попытку, которая
// могла дойти. Без исхода попытки состояние меняет только явный повтор
// failed → pending. Отправленное держит telegramOutboxFence, неизвестный исход
// — telegramOutboxUnconfirmed: их записи триггер не разбирает, и отказ
// называет своё ограждение.
const telegramOutboxAttempts = `CREATE TRIGGER telegram_outbox_attempts BEFORE UPDATE ON telegram_outbox WHEN OLD.state <> 'sent' AND NOT (OLD.state = 'unconfirmed' AND NEW.state IN ('failed', 'pending')) AND ((NEW.attempts > OLD.attempts AND (OLD.attempts > OLD.settled_attempts OR NEW.settled_attempts IS NOT OLD.settled_attempts)) OR (NEW.settled_attempts > OLD.settled_attempts AND NEW.state = 'pending') OR (NEW.state IS NOT OLD.state AND NEW.settled_attempts = OLD.settled_attempts AND NOT (OLD.state = 'failed' AND NEW.state = 'pending'))) BEGIN SELECT RAISE(ABORT, 'telegram notification attempt is out of order'); END`

// telegramOutboxUnconfirmed: неизвестный исход не становится failed и не
// возвращается в очередь, откуда следующая попытка могла бы записать failed:
// прежняя попытка могла дойти (как delivery запроса, #260).
const telegramOutboxUnconfirmed = `CREATE TRIGGER telegram_outbox_unconfirmed BEFORE UPDATE OF state ON telegram_outbox WHEN OLD.state = 'unconfirmed' AND NEW.state IN ('failed', 'pending') BEGIN SELECT RAISE(ABORT, 'unconfirmed telegram notification may have been sent'); END`

// telegramOutboxNoReplace: вставка не занимает место существующей строки:
// INSERT OR REPLACE стёр бы отправленное уведомление мимо ограждения
// обновления и поставил бы его в очередь заново.
const telegramOutboxNoReplace = `CREATE TRIGGER telegram_outbox_no_replace BEFORE INSERT ON telegram_outbox WHEN EXISTS (SELECT 1 FROM telegram_outbox WHERE id = NEW.id) BEGIN SELECT RAISE(ABORT, 'telegram notification already exists'); END`

// telegramOutboxNoDelete: уведомление в очереди (pending, и впервые, и
// повтором failed) и попытка в полёте не удаляются: удаление стёрло бы то,
// что ещё должно уйти, или запись о попытке, которая могла дойти. Уведомление
// с записанным исходом удаляет срок хранения отправителя: иначе таблица росла
// бы без предела, а снимок базы выкладки ограничен (DEPLOY.md, verify).
// Уведомление, которое некуда отправить, отправитель сначала закрывает
// синтетической попыткой с исходом failed (MODEL.md, требование к #282).
// Каскад удаления пользователя проходит: к моменту удаления дочерних строк
// родителя в users уже нет, как у swarm_requests_no_delete.
const telegramOutboxNoDelete = `CREATE TRIGGER telegram_outbox_no_delete BEFORE DELETE ON telegram_outbox WHEN EXISTS (SELECT 1 FROM users WHERE id = OLD.user_id) AND (OLD.state = 'pending' OR OLD.attempts > OLD.settled_attempts) BEGIN SELECT RAISE(ABORT, 'telegram notification is queued or in flight'); END`

// telegramOutboxFreshID: явный номер не больше счётчика AUTOINCREMENT мог
// принадлежать удалённому уведомлению, и новое под ним выглядело бы им;
// AUTOINCREMENT держит только номера, которые выбирает сам. Триггер AFTER: в
// BEFORE INSERT номер, который назначит SQLite, ещё не определён (NEW.id там
// -1), а в AFTER INSERT он настоящий, и sqlite_sequence ещё держит счётчик до
// оператора: SQLite записывает его в конце оператора. Номер дальше счётчика
// ничей и проходит. Запись мимо самого счётчика (sqlite_sequence) ограждение
// не держит.
const telegramOutboxFreshID = `CREATE TRIGGER telegram_outbox_fresh_id AFTER INSERT ON telegram_outbox WHEN NEW.id <= (SELECT seq FROM sqlite_sequence WHERE name = 'telegram_outbox') BEGIN SELECT RAISE(ABORT, 'telegram notification id is not new'); END`

// telegramObjects — индексы и триггеры outbox в порядке создания. Триггер
// отзыва сессии создаётся последним, своим текстом версии
// (telegramSchemaWith).
var telegramObjects = []struct{ kind, name, sql string }{
	{"index", "telegram_outbox_pending", telegramOutboxPending},
	{"index", "telegram_outbox_user", telegramOutboxUser},
	{"trigger", "telegram_outbox_insert", telegramOutboxInsert},
	{"trigger", "telegram_outbox_fence", telegramOutboxFence},
	{"trigger", "telegram_outbox_attempts", telegramOutboxAttempts},
	{"trigger", "telegram_outbox_unconfirmed", telegramOutboxUnconfirmed},
	{"trigger", "telegram_outbox_no_replace", telegramOutboxNoReplace},
	{"trigger", "telegram_outbox_no_delete", telegramOutboxNoDelete},
	{"trigger", "telegram_outbox_fresh_id", telegramOutboxFreshID},
}

// telegramFullSchema — таблицы, индекс и триггеры Telegram. Колонка вида
// сессии входит в schema.sql свежей базы, а в мигрированную — шагом 28→29.
func telegramFullSchema() string {
	return telegramSchemaWith(userTelegramLinksSchema, sessionTelegramRevoke)
}

// telegramSchemaV29 — DDL Telegram шага 28→29: таблица привязок без роя по
// умолчанию и имени собеседника, триггер отзыва без имени, прочее — текст
// реестра, который с версии 29 не менялся.
func telegramSchemaV29() string {
	return telegramSchemaWith(userTelegramLinksSchemaV29, sessionTelegramRevokeV29)
}

// telegramSchemaWith — таблица привязок links, outbox, индексы и триггеры
// outbox, затем триггер отзыва сессии revoke.
func telegramSchemaWith(links, revoke string) string {
	statements := links + ";" + telegramOutboxSchema
	for _, object := range telegramObjects {
		statements += ";" + object.sql
	}
	return statements + ";" + revoke
}

// verifyTelegramSchema сверяет таблицы Telegram, индекс очереди и
// ограждения.
func verifyTelegramSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, userTelegramLinksSchema); err != nil {
		return err
	}
	if err := checkSchema(ctx, tx, telegramOutboxSchema); err != nil {
		return err
	}
	for _, object := range telegramObjects {
		if err := checkObject(ctx, tx, object.kind, object.name, object.sql); err != nil {
			return err
		}
	}
	return checkObject(ctx, tx, "trigger", "sessions_telegram_revoke", sessionTelegramRevoke)
}
