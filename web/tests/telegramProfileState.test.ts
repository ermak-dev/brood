// FEATURE: What the Telegram section of the profile shows and offers (#285, API #281).
// SPEC: The badge says whether the section needs a look: an error is a
// problem, a code waiting for /start waits for the user, a linked or idle
// profile needs nothing. A link code is issued only through a known bot that
// runs: not to a linked chat, not while the installation bot is stopped for a
// user without an own bot, not without any bot. The one destructive action
// removes the own bot (and with it the chat link) or, through the
// installation bot, unlinks the chat. A code shown on the page stays shown
// while the server still waits for exactly it: the server decides with its
// own clock, a skewed browser clock does not. The countdown is in whole
// minutes on the server clock, which the Date of a reply gives. The page
// polls only while a code waits. Subscriptions list the known kinds in the
// server's order and keep kinds of a newer server. A token is checked for the
// shape BotFather issues before it is sent.
import { describe, expect, spyOn, test } from 'bun:test';
import {
  TELEGRAM_EVENTS,
  type TelegramLinkCode,
  type TelegramProfile,
} from '@/services/brood/telegram';
import {
  EMPTY_DRAFT,
  codeCountdown,
  liveCode,
  linkBlocker,
  removal,
  serverClockOffset,
  subscriptionRows,
  telegramTone,
  tokenDraft,
  tokenProblem,
  waitsForStart,
  withIssuedCode,
} from '@/features/profile/telegramState';

const NOW = Date.parse('2026-11-14T10:00:00Z');
const IN_15 = '2026-11-14T10:15:00Z';

const profile = (over: Partial<TelegramProfile> = {}): TelegramProfile => ({
  botUsername: 'brood_bot',
  ownBot: false,
  state: 'unlinked',
  events: [...TELEGRAM_EVENTS],
  chat: null,
  linkCodePending: false,
  ...over,
});

const linked = (over: Partial<TelegramProfile> = {}) =>
  profile({
    state: 'linked',
    chat: { telegramUserId: 4242, chatId: 4242, username: 'alice_tg' },
    ...over,
  });

const issued: TelegramLinkCode = {
  code: 'Zk3vQx2b8m1nP0aR7sT4uV6wX9yZ5cD3eF8gH1iJ2kL',
  expiresAt: IN_15,
  botUsername: 'brood_bot',
  url: 'https://t.me/brood_bot?start=Zk3vQx2b8m1nP0aR7sT4uV6wX9yZ5cD3eF8gH1iJ2kL',
};

describe('badge tone', () => {
  test('an error is a problem, a waiting code waits for the user, the rest needs nothing', () => {
    expect(telegramTone(profile({ state: 'error', reason: 'x' }))).toBe('problem');
    expect(telegramTone(profile({ linkCodePending: true, linkCodeExpiresAt: IN_15 }))).toBe(
      'attention'
    );
    expect(telegramTone(linked())).toBe('neutral');
    expect(telegramTone(profile())).toBe('neutral');
    // Состояние сервера новее панели не выдаётся за проблему.
    expect(telegramTone(profile({ state: 'paused' }))).toBe('neutral');
  });

  test('whether a code waits is the server word, not the browser clock', () => {
    // Часы браузера ушли вперёд срока, а сервер своими часами код ждёт.
    const pending = profile({ linkCodePending: true, linkCodeExpiresAt: '2026-11-14T09:59:59Z' });
    expect(telegramTone(pending)).toBe('attention');
    expect(waitsForStart(pending)).toBe(true);
    // Сервер код больше не ждёт: срок впереди по часам браузера не спорит.
    const gone = profile({ linkCodePending: false, linkCodeExpiresAt: IN_15 });
    expect(telegramTone(gone)).toBe('neutral');
    expect(waitsForStart(gone)).toBe(false);
  });
});

describe('link code issue', () => {
  test('a known running bot gives a code to an unlinked or failed profile', () => {
    expect(linkBlocker(profile())).toBeNull();
    expect(linkBlocker(profile({ ownBot: true, botUsername: 'alice_dev_bot' }))).toBeNull();
    expect(linkBlocker(profile({ ownBot: true, state: 'error', reason: 'x' }))).toBeNull();
  });

  test('a linked chat is unlinked first', () => {
    expect(linkBlocker(linked())).toBe('linked');
  });

  test('a stopped installation bot blocks only users without an own bot', () => {
    expect(linkBlocker(profile({ botReason: 'Telegram rejected the bot token (401)' }))).toBe(
      'install_stopped'
    );
    expect(
      linkBlocker(
        profile({ ownBot: true, botUsername: 'alice_dev_bot', botReason: 'stopped (401)' })
      )
    ).toBeNull();
  });

  test('without an own bot and without a known installation bot there is no bot to link through', () => {
    expect(linkBlocker(profile({ botUsername: '' }))).toBe('no_bot');
  });
});

describe('removal', () => {
  test('an own bot is removed together with its chat link', () => {
    expect(removal(profile({ ownBot: true }))).toBe('remove_bot');
    expect(removal(linked({ ownBot: true }))).toBe('remove_bot');
  });

  test('through the installation bot only a linked chat is unlinked', () => {
    expect(removal(linked())).toBe('unlink');
    expect(removal(profile())).toBeNull();
    expect(removal(profile({ linkCodePending: true, linkCodeExpiresAt: IN_15 }))).toBeNull();
  });
});

describe('a code shown on the page', () => {
  test('right after the issue the profile waits for exactly this code', () => {
    const next = withIssuedCode(profile({ state: 'error', reason: 'Telegram rejected' }), issued);
    expect(next.state).toBe('unlinked');
    expect(next.reason).toBeUndefined();
    expect(next.linkCodePending).toBe(true);
    expect(next.linkCodeExpiresAt).toBe(IN_15);
    expect(liveCode(issued, next)).toBe(issued);
  });

  test('it is gone once linked, replaced elsewhere or no longer waited for', () => {
    const waiting = withIssuedCode(profile(), issued);
    expect(liveCode(issued, linked({ linkCodeExpiresAt: IN_15 }))).toBeNull();
    expect(liveCode(issued, { ...waiting, linkCodeExpiresAt: '2026-11-14T10:16:00Z' })).toBeNull();
    // Истёкший код сервер больше не ждёт: это его слово, а не часов браузера.
    expect(liveCode(issued, { ...waiting, linkCodePending: false })).toBeNull();
    expect(liveCode(null, waiting)).toBeNull();
  });

  test('a browser clock past the expiry does not hide a code the server waits for', () => {
    const clock = spyOn(Date, 'now').mockReturnValue(Date.parse(IN_15) + 20 * 60_000);
    try {
      const waiting = withIssuedCode(profile(), issued);
      expect(waitsForStart(waiting)).toBe(true);
      expect(liveCode(issued, waiting)).toBe(issued);
    } finally {
      clock.mockRestore();
    }
  });

  test('the same moment in another form is the same code', () => {
    const waiting = {
      ...withIssuedCode(profile(), issued),
      linkCodeExpiresAt: '2026-11-14T10:15:00.000Z',
    };
    expect(liveCode(issued, waiting)).toBe(issued);
  });
});

describe('countdown', () => {
  test('whole minutes left, rounded to the nearest', () => {
    expect(codeCountdown(IN_15, NOW)).toEqual({ expired: false, minutes: 15 });
    expect(codeCountdown(IN_15, NOW + 5_000)).toEqual({ expired: false, minutes: 15 });
    // 1 минута 29 секунд.
    expect(codeCountdown(IN_15, NOW + 13 * 60_000 + 31_000)).toEqual({
      expired: false,
      minutes: 1,
    });
  });

  test('under a minute is zero minutes, the expiry itself is expired', () => {
    expect(codeCountdown(IN_15, NOW + 14 * 60_000 + 1_000)).toEqual({ expired: false, minutes: 0 });
    expect(codeCountdown(IN_15, Date.parse(IN_15))).toEqual({ expired: true, minutes: 0 });
  });

  test('a broken moment has no countdown', () => {
    expect(codeCountdown('soon', NOW)).toBeNull();
    expect(codeCountdown(undefined, NOW)).toBeNull();
  });
});

describe('polling', () => {
  test('only a code the server waits for is polled, until the server stops waiting', () => {
    const waiting = profile({ linkCodePending: true, linkCodeExpiresAt: IN_15 });
    expect(waitsForStart(waiting)).toBe(true);
    expect(waitsForStart(profile())).toBe(false);
    expect(waitsForStart(linked({ linkCodePending: true, linkCodeExpiresAt: IN_15 }))).toBe(false);
    // Ошибка ждёт действия пользователя, а не /start.
    expect(waitsForStart({ ...waiting, state: 'error', reason: 'x' })).toBe(false);
    expect(waitsForStart({ ...waiting, linkCodeExpiresAt: undefined })).toBe(false);
  });
});

describe('server clock', () => {
  test('the server moment of a reply and the browser moment it came give the offset', () => {
    const received = Date.parse('2026-11-14T10:20:00.400Z');
    // Часы браузера на 20 минут впереди сервера.
    expect(serverClockOffset('Sat, 14 Nov 2026 10:00:00 GMT', received)).toBe(-(20 * 60_000) - 400);
    expect(serverClockOffset('Sat, 14 Nov 2026 10:20:00 GMT', received)).toBe(-400);
  });

  test('a reply without a readable Date keeps the browser clock', () => {
    const received = Date.parse('2026-11-14T10:20:00Z');
    expect(serverClockOffset(null, received)).toBe(0);
    expect(serverClockOffset('', received)).toBe(0);
    expect(serverClockOffset('soon', received)).toBe(0);
  });
});

describe('subscriptions', () => {
  test('known kinds in the server order, each on or off', () => {
    const rows = subscriptionRows(['step_failed', 'request_opened']);
    expect(rows.map((row) => row.kind)).toEqual([...TELEGRAM_EVENTS]);
    expect(rows.filter((row) => row.on).map((row) => row.kind)).toEqual([
      'request_opened',
      'step_failed',
    ]);
    expect(rows.every((row) => row.known)).toBe(true);
  });

  test('a kind of a newer server is kept and marked unknown', () => {
    const rows = subscriptionRows(['request_opened', 'swarm_deleted']);
    expect(rows.at(-1)).toEqual({ kind: 'swarm_deleted', on: true, known: false });
    expect(rows).toHaveLength(TELEGRAM_EVENTS.length + 1);
  });
});

describe('token shape', () => {
  test('a BotFather token passes, surrounding spaces of a paste included', () => {
    expect(tokenProblem('123456789:AAH-synthetic_bot_token')).toBeNull();
    expect(tokenProblem('  123456789:AAH-synthetic_bot_token\n')).toBeNull();
  });

  test('the page keeps of a typed token only whether it is there and its shape', () => {
    expect(EMPTY_DRAFT).toEqual({ filled: false, problem: null });
    expect(tokenDraft('')).toEqual(EMPTY_DRAFT);
    expect(tokenDraft('  \n')).toEqual(EMPTY_DRAFT);
    expect(tokenDraft('  123456789:AAH-synthetic_bot_token\n')).toEqual({
      filled: true,
      problem: null,
    });
    expect(tokenDraft('not-a-token')).toEqual({ filled: true, problem: 'shape' });
    expect(JSON.stringify(tokenDraft('123456789:AAH-synthetic_bot_token'))).not.toContain(
      'synthetic'
    );
  });

  test('an empty field says nothing; anything else is the wrong shape', () => {
    expect(tokenProblem('')).toBeNull();
    expect(tokenProblem('   ')).toBeNull();
    for (const value of ['123456789', 'bot123:abc', '123:abc def', '123:', ':abc', '12:ab/cd']) {
      expect({ value, problem: tokenProblem(value) }).toEqual({ value, problem: 'shape' });
    }
  });
});
