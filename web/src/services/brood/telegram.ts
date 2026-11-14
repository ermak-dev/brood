/**
 * brood: профиль Telegram вошедшего пользователя (#285, API #281).
 *
 * GET /api/me/telegram читает профиль; PUT принимает ровно {"token"} и
 * отвечает профилем; PUT /api/me/telegram/events принимает ровно {"events"},
 * полный набор подписок, и отвечает профилем (#296, API #295); DELETE
 * снимает свой бот и привязку чата; POST /api/me/telegram/link выдаёт
 * одноразовый код привязки. Маршруты действуют только для самого
 * вызывающего, за сессией и CSRF. Токен бота write-only:
 * его не несёт ни ответ, ни отказ, и здесь он нигде не хранится. Отказ --
 * {"detail", "reason"} с одной публичной причиной. Каждый результат несёт
 * заголовок Date ответа: срок кода назначен часами сервера.
 */

import { requestWithHeaders } from './http';

/** Виды уведомлений, на которые подписывается пользователь, в порядке
 * сервера (store.TelegramEventKinds). */
export const TELEGRAM_EVENTS = [
  'request_opened',
  'request_answered',
  'answer_delivery',
  'pass_started',
  'pass_finished',
  'step_failed',
  'actor_question',
] as const;

/** Привязанный личный чат. Имя собеседника без @ сервер берёт из строки
 * привязки (#294): имя из /start с кодом, затем из последнего сообщения или
 * нажатия кнопки чата; перезапуск сервера его не снимает. Нет имени -- нет
 * поля. */
export interface TelegramChat {
  telegramUserId: number;
  chatId: number;
  username?: string;
}

export interface TelegramProfile {
  /** Бот привязки: свой или, без него, бот установки; пусто, пока имя бота
   * не известно или бота нет. */
  botUsername: string;
  /** Задан ли свой токен. */
  ownBot: boolean;
  /** Почему остановлен бот установки. */
  botReason?: string;
  /** `unlinked`, `linked` или `error`; состояние сервера новее панели
   * остаётся как пришло. */
  state: string;
  /** Публичная причина состояния `error`. */
  reason?: string;
  /** Виды уведомлений, на которые подписан пользователь. */
  events: string[];
  chat: TelegramChat | null;
  /** Выданный раньше код ещё ждёт /start. */
  linkCodePending: boolean;
  linkCodeExpiresAt?: string;
}

/** Код привязки показывается один раз: сервер хранит только его хэш. */
export interface TelegramLinkCode {
  code: string;
  expiresAt: string;
  botUsername: string;
  /** https://t.me/<бот>?start=<код>, когда имя бота известно. */
  url?: string;
}

const INVALID_PROFILE = 'Invalid telegram profile response';
const INVALID_CODE = 'Invalid telegram link code response';
const PROFILE_PATH = '/api/me/telegram';

const isRecord = (value: unknown): value is Record<string, unknown> =>
  Boolean(value) && typeof value === 'object' && !Array.isArray(value);

function optionalString(item: Record<string, unknown>, key: string, invalid: string) {
  const value = item[key];
  if (value === undefined || value === null || value === '') return undefined;
  if (typeof value !== 'string') throw new Error(invalid);
  return value;
}

function parseChat(value: unknown): TelegramChat | null {
  if (value === null || value === undefined) return null;
  if (
    !isRecord(value) ||
    !Number.isSafeInteger(value.telegram_user_id) ||
    !Number.isSafeInteger(value.chat_id)
  ) {
    throw new Error(INVALID_PROFILE);
  }
  const chat: TelegramChat = {
    telegramUserId: value.telegram_user_id as number,
    chatId: value.chat_id as number,
  };
  const username = optionalString(value, 'username', INVALID_PROFILE);
  if (username) chat.username = username;
  return chat;
}

/** Профиль с провода: брак ответа -- громкая ошибка, а не состояние,
 * которого сервер не присылал. */
export function parseTelegramProfile(value: unknown): TelegramProfile {
  if (
    !isRecord(value) ||
    typeof value.bot_username !== 'string' ||
    typeof value.own_bot !== 'boolean' ||
    typeof value.state !== 'string' ||
    value.state === '' ||
    typeof value.link_code_pending !== 'boolean' ||
    !Array.isArray(value.events) ||
    !value.events.every((event) => typeof event === 'string')
  ) {
    throw new Error(INVALID_PROFILE);
  }
  const profile: TelegramProfile = {
    botUsername: value.bot_username,
    ownBot: value.own_bot,
    state: value.state,
    events: [...value.events],
    chat: parseChat(value.chat),
    linkCodePending: value.link_code_pending,
  };
  for (const [wire, key] of [
    ['bot_reason', 'botReason'],
    ['reason', 'reason'],
    ['link_code_expires_at', 'linkCodeExpiresAt'],
  ] as const) {
    const field = optionalString(value, wire, INVALID_PROFILE);
    if (field) profile[key] = field;
  }
  return profile;
}

/** Ссылка только на t.me: панель ставит её в href. */
const telegramLink = (url: string | undefined) =>
  url?.startsWith('https://t.me/') ? url : undefined;

export function parseTelegramLinkCode(value: unknown): TelegramLinkCode {
  if (
    !isRecord(value) ||
    typeof value.code !== 'string' ||
    value.code === '' ||
    typeof value.expires_at !== 'string' ||
    value.expires_at === '' ||
    typeof value.bot_username !== 'string'
  ) {
    throw new Error(INVALID_CODE);
  }
  const code: TelegramLinkCode = {
    code: value.code,
    expiresAt: value.expires_at,
    botUsername: value.bot_username,
  };
  const url = telegramLink(optionalString(value, 'url', INVALID_CODE));
  if (url) code.url = url;
  return code;
}

/** Результат вместе с часами сервера. */
export interface Served<T> {
  data: T;
  /** Заголовок Date ответа; null -- его нет. */
  date: string | null;
}

async function served<T>(
  parse: (value: unknown) => T,
  method: string,
  url: string,
  body?: unknown
): Promise<Served<T>> {
  const { data, headers } = await requestWithHeaders<unknown>(method, url, body);
  return { data: parse(data), date: headers.get('Date') };
}

export const telegramApi = {
  profile: () => served(parseTelegramProfile, 'GET', PROFILE_PATH),
  /** Токен уходит на сервер и больше никуда: ответ -- профиль без него. */
  setToken: (token: string) => served(parseTelegramProfile, 'PUT', PROFILE_PATH, { token }),
  /** Подписки целиком: запись заменяет набор, ответ -- профиль. */
  setEvents: (events: string[]) =>
    served(parseTelegramProfile, 'PUT', `${PROFILE_PATH}/events`, { events }),
  remove: () => served(parseTelegramProfile, 'DELETE', PROFILE_PATH),
  issueLinkCode: () => served(parseTelegramLinkCode, 'POST', `${PROFILE_PATH}/link`),
};
