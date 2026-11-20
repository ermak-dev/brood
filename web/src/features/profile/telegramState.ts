/**
 * Чистая логика секции Telegram профиля (#285, API #281): что показать и
 * что предложить по профилю сервера. Часов здесь нет: ждёт ли код, решает
 * сервер своими часами (link_code_pending), а момент «сейчас» для отсчёта
 * срока передаёт страница, уже с поправкой к часам сервера.
 */
import type { BadgeTone } from '@/features/brood/ui';
import type { TelegramLinkCode, TelegramProfile } from '@/services/brood/telegram';

/** Момент в миллисекундах; брак -- NaN. */
const moment = (value: string | undefined) => (value ? Date.parse(value) : Number.NaN);

/** Ждёт ли сервер /start с выданным кодом: привязки ещё нет, и сервер
 * говорит, что код ждёт, со сроком. Истечение решают часы сервера, а не
 * браузера: часы браузера, ушедшие вперёд, прятали бы живой код и
 * останавливали опрос раньше срока. Ошибка привязки ждёт действия
 * пользователя, а не кода. */
export function waitsForStart(profile: TelegramProfile): boolean {
  if (profile.state !== 'unlinked' || !profile.linkCodePending) return false;
  return Number.isFinite(moment(profile.linkCodeExpiresAt));
}

/** Тон бейджа (#254): ошибка -- проблема, ждущий код -- ждёт человека,
 * привязанный или пустой профиль ничего не требует. */
export function telegramTone(profile: TelegramProfile): BadgeTone {
  if (profile.state === 'error') return 'problem';
  return waitsForStart(profile) ? 'attention' : 'neutral';
}

/** Почему код привязки не выдаётся: чат уже привязан, бот установки
 * остановлен у пользователя без своего бота, бота нет вовсе. null --
 * выдаётся. Окончательно решает сервер. */
export type LinkBlocker = 'linked' | 'install_stopped' | 'no_bot';

export function linkBlocker(profile: TelegramProfile): LinkBlocker | null {
  if (profile.state === 'linked') return 'linked';
  if (profile.ownBot) return null;
  if (profile.botReason) return 'install_stopped';
  return profile.botUsername ? null : 'no_bot';
}

/** Единственное разрушительное действие: DELETE снимает свой бот вместе с
 * привязкой чата, а у пользователя бота установки -- только привязку. */
export type Removal = 'remove_bot' | 'unlink';

export function removal(profile: TelegramProfile): Removal | null {
  if (profile.ownBot) return 'remove_bot';
  return profile.state === 'linked' ? 'unlink' : null;
}

/** Профиль сразу после выдачи кода: сервер снял ошибку привязки и ждёт
 * этот код. Опрос подтвердит, страница не мигает до него. */
export function withIssuedCode(profile: TelegramProfile, code: TelegramLinkCode): TelegramProfile {
  const next: TelegramProfile = {
    ...profile,
    state: 'unlinked',
    linkCodePending: true,
    linkCodeExpiresAt: code.expiresAt,
  };
  delete next.reason;
  return next;
}

/** Код этой страницы, пока сервер ждёт именно его: не привязан, код ждёт,
 * и срок совпадает со сроком ждущего кода. Иначе код заменён (другая
 * вкладка), снят или истёк по часам сервера, и показывать его незачем. */
export function liveCode(
  code: TelegramLinkCode | null,
  profile: TelegramProfile
): TelegramLinkCode | null {
  if (!code || !waitsForStart(profile)) return null;
  return moment(code.expiresAt) === moment(profile.linkCodeExpiresAt) ? code : null;
}

/** Поправка к часам браузера: часы сервера по заголовку Date ответа минус
 * момент браузера, когда ответ пришёл. Без читаемого Date -- 0, часы
 * браузера как есть. Точность Date -- секунда: для отсчёта минут хватает. */
export function serverClockOffset(date: string | null, receivedMs: number): number {
  const server = moment(date ?? undefined);
  return Number.isFinite(server) ? server - receivedMs : 0;
}

/** Сколько осталось до срока кода: целые минуты с округлением до
 * ближайшей, 0 -- меньше минуты. null -- срок не прочитался. */
export interface CodeCountdown {
  expired: boolean;
  minutes: number;
}

export function codeCountdown(expiresAt: string | undefined, nowMs: number): CodeCountdown | null {
  const expires = moment(expiresAt);
  if (!Number.isFinite(expires)) return null;
  const left = expires - nowMs;
  if (left <= 0) return { expired: true, minutes: 0 };
  return { expired: false, minutes: left < 60_000 ? 0 : Math.round(left / 60_000) };
}

/** Форма токена BotFather, как её проверяет клиент Bot API сервера. */
const TOKEN_SHAPE = /^[0-9]{1,20}:[A-Za-z0-9_-]{1,128}$/;

/** Токен поля без пробелов вставки по краям. */
export const tokenValue = (draft: string) => draft.trim();

/** Что не так с токеном поля: пустое поле ничего не говорит (сохранить
 * нечего), иное -- не та форма. */
export function tokenProblem(draft: string): 'shape' | null {
  const token = tokenValue(draft);
  return token === '' || TOKEN_SHAPE.test(token) ? null : 'shape';
}

/** Что страница держит о набранном токене, не держа сам токен: есть ли что
 * сохранить и не та ли форма. */
export interface TokenDraft {
  filled: boolean;
  problem: 'shape' | null;
}

export const EMPTY_DRAFT: TokenDraft = { filled: false, problem: null };

export function tokenDraft(draft: string): TokenDraft {
  return { filled: tokenValue(draft) !== '', problem: tokenProblem(draft) };
}
