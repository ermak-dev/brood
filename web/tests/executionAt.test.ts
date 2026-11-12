// FEATURE: The swarm row says when its execution last changed (#268, epic
// #190).
// SPEC: The row of GET /api/projects/{id}/swarms carries `executionAt`, the
// moment of the latest execution change (the start of the current pass, a
// stop or the end of the last pass), RFC 3339 UTC with fractional seconds.
// The field is absent for a swarm without a pass and for some older passes.
// The panel reads it as an instant: anything but an RFC 3339 timestamp with
// a zone is malformed and lost alone, without hiding the execution summary.
// How long ago it was is phrased by the locale: whole minutes, hours or days,
// never rounded up; under a minute, and a moment ahead of the browser clock
// (clock skew), is "just now", never "in 2 minutes".
import { describe, expect, test } from 'bun:test';
import type { AccessSwarm } from '@/services/brood/access';
import { executionMoment, formatSince } from '@/features/access/executionAt';
import { swarmActivityOf } from '@/features/access/swarmActivity';
import { DAY_MS, HOUR_MS, MINUTE_MS } from '@/utils/time/durations';

const swarm = (over: Partial<AccessSwarm> = {}): AccessSwarm => ({
  id: 's1',
  project_id: 'p1',
  owner_id: 'u1',
  owner_username: 'alice',
  role: 'owner',
  name: 'crew',
  atespace_name: 'space-s1',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-01T09:00:00Z',
  execution: 'succeeded',
  openRequests: 0,
  ...over,
});

/** Значение поля провода, которое тип строки не допускает: брак сервера. */
const odd = (value: unknown) => value as never;

const at = Date.UTC(2026, 10, 12, 10, 15, 30, 123);

describe('executionMoment', () => {
  test('reads an RFC 3339 instant with or without fractional seconds', () => {
    // Go пишет до девяти знаков долей секунды; браузер их не все разбирает.
    expect(executionMoment('2026-11-12T10:15:30.123456789Z')).toBe(at);
    expect(executionMoment('2026-11-12T10:15:30.123Z')).toBe(at);
    expect(executionMoment('2026-11-12T10:15:30Z')).toBe(at - 123);
  });

  test('an offset names the same instant as UTC', () => {
    expect(executionMoment('2026-11-12T13:15:30.123+03:00')).toBe(at);
  });

  test('anything else is malformed, not a moment', () => {
    for (const bad of [
      undefined,
      null,
      at,
      {},
      '',
      '   ',
      'yesterday',
      'Nov 12 2026 10:15:30 GMT',
      '2026-11-12',
      // Без зоны момент зависел бы от часового пояса браузера.
      '2026-11-12T10:15:30',
      '2026-13-12T10:15:30Z',
    ]) {
      expect([bad, executionMoment(bad)]).toEqual([bad, undefined]);
    }
  });
});

describe('formatSince', () => {
  const now = at + DAY_MS;

  test('under a minute and a moment ahead of the browser clock read as just now', () => {
    expect(formatSince(now, now, 'en')).toBeNull();
    expect(formatSince(now - MINUTE_MS + 1, now, 'en')).toBeNull();
    expect(formatSince(now + 30_000, now, 'en')).toBeNull();
    expect(formatSince(now + 2 * HOUR_MS, now, 'en')).toBeNull();
  });

  test('whole minutes, hours and days, never rounded up past a boundary', () => {
    expect(formatSince(now - MINUTE_MS, now, 'en')).toBe('1 minute ago');
    expect(formatSince(now - 5 * MINUTE_MS - 59_000, now, 'en')).toBe('5 minutes ago');
    expect(formatSince(now - HOUR_MS + 1, now, 'en')).toBe('59 minutes ago');
    expect(formatSince(now - HOUR_MS, now, 'en')).toBe('1 hour ago');
    expect(formatSince(now - DAY_MS + 1, now, 'en')).toBe('23 hours ago');
    expect(formatSince(now - DAY_MS, now, 'en')).toBe('1 day ago');
    expect(formatSince(now - 3 * DAY_MS - HOUR_MS, now, 'en')).toBe('3 days ago');
  });

  test('the phrase follows the locale of the panel', () => {
    const five = now - 5 * MINUTE_MS;
    expect(formatSince(five, now, 'ru')).toBe('5 минут назад');
    expect(formatSince(five, now, 'zh-CN')).toBe('5分钟前');
    expect(formatSince(five, now, 'ko')).toBe('5분 전');
  });
});

describe('swarmActivityOf and executionAt', () => {
  test('the summary carries the moment of the row', () => {
    expect(
      swarmActivityOf(swarm({ execution: 'running', executionAt: '2026-11-12T10:15:30.123Z' }))
    ).toEqual({ execution: 'running', openRequests: 0, executionAt: at });
  });

  test('a row of an older server, or of a swarm without a pass, has no moment', () => {
    const activity = swarmActivityOf(swarm({ execution: 'idle' }));
    expect(activity).toEqual({ execution: 'idle', openRequests: 0 });
    expect(activity?.executionAt).toBeUndefined();
  });

  test('a malformed moment is lost alone, not the summary', () => {
    for (const bad of ['2026-11-12', 'later', 1731406530123, null]) {
      expect(swarmActivityOf(swarm({ undeliveredAnswers: 1, executionAt: odd(bad) }))).toEqual({
        execution: 'succeeded',
        openRequests: 0,
        undeliveredAnswers: 1,
      });
    }
  });

  test('a moment without a summary does not make one', () => {
    expect(
      swarmActivityOf(swarm({ execution: undefined, executionAt: '2026-11-12T10:15:30Z' }))
    ).toBeUndefined();
  });
});
