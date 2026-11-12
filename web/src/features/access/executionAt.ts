/**
 * Момент последнего исполнения роя (#268): строка
 * `GET /api/projects/{id}/swarms` несёт `executionAt` -- начало текущего
 * прохода, остановку или окончание последнего, RFC 3339 UTC с долями
 * секунды. Список роёв и «Последние прогоны» обзора показывают, сколько с
 * него прошло, а точный момент -- в подсказке.
 *
 * Сколько прошло, считается от переданного `nowMs`: часов здесь нет, и
 * границы единиц проверяются напрямую.
 */
import { parseTimestampMs } from '@/utils/timestamp';
import { formatRelativeInstant } from '@/utils/quota/relativeTime';
import { MINUTE_MS } from '@/utils/time/durations';

// Дата, время, доли секунды любой длины и обязательная зона: без зоны
// момент зависел бы от часового пояса браузера, а «2026-11-12» -- не момент.
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/i;

/** Момент из поля строки в миллисекундах; иное значение -- брак, undefined. */
export function executionMoment(value: unknown): number | undefined {
  if (typeof value !== 'string' || !RFC3339.test(value)) return undefined;
  const ms = parseTimestampMs(value);
  return Number.isFinite(ms) ? ms : undefined;
}

/** Сколько прошло с момента, словами локали: целые минуты, часы или дни,
 * без округления вверх («59 минут», а не «1 час»). null -- «только что»:
 * меньше минуты, в том числе момент впереди часов браузера (часы сервера
 * и браузера расходятся), а не «через 2 минуты». */
export function formatSince(atMs: number, nowMs: number, locale?: string): string | null {
  if (nowMs - atMs < MINUTE_MS) return null;
  return formatRelativeInstant(atMs, nowMs, locale);
}
