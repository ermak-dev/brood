// FEATURE: Typed client of the Telegram profile API (#285, API #281, epic #278).
// SPEC: GET /api/me/telegram reads the profile of the caller: the bot the link
// goes through (own or the installation bot), whether an own token is set,
// the state with its public reason, the subscriptions, the linked chat and
// whether a link code waits. PUT takes exactly {"token"} and answers with the
// profile; DELETE removes the own bot and the chat link; POST .../link issues
// a one-time code with its t.me deep link. The token is write-only: no
// result the client hands to the panel carries it, even when a server echoes
// it in fields the contract does not have. Every result comes with the Date
// of its reply, the server clock the page counts a code down by. Writes send
// the CSRF token; a refusal carries the server's public reason.
import { afterEach, describe, expect, test } from 'bun:test';
import {
  TELEGRAM_EVENTS,
  parseTelegramLinkCode,
  parseTelegramProfile,
  telegramApi,
} from '@/services/brood/telegram';
import { BroodApiError } from '@/services/brood/http';

const TOKEN = '123456789:AAH-synthetic_bot_token_for_tests_only';
const SECRET = TOKEN.split(':')[1];
/** Лишние поля с токеном, которых контракт не допускает. */
const echoed = { token: TOKEN, bot_token: TOKEN, botToken: TOKEN, token_secret: SECRET };
/** Поля профиля панели, которые есть всегда. */
const PROFILE_KEYS = ['botUsername', 'chat', 'events', 'linkCodePending', 'ownBot', 'state'];

const unlinkedWire = {
  bot_username: 'brood_bot',
  own_bot: false,
  state: 'unlinked',
  events: [...TELEGRAM_EVENTS],
  chat: null,
  link_code_pending: false,
};

const linkedWire = {
  bot_username: 'alice_dev_bot',
  own_bot: true,
  state: 'linked',
  events: ['request_opened', 'step_failed'],
  chat: { telegram_user_id: 4242, chat_id: 4242, username: 'alice_tg' },
  link_code_pending: false,
};

describe('profile from the wire', () => {
  test('snake_case fields become the panel profile', () => {
    expect(parseTelegramProfile(unlinkedWire)).toEqual({
      botUsername: 'brood_bot',
      ownBot: false,
      state: 'unlinked',
      events: [...TELEGRAM_EVENTS],
      chat: null,
      linkCodePending: false,
    });
    expect(parseTelegramProfile(linkedWire)).toEqual({
      botUsername: 'alice_dev_bot',
      ownBot: true,
      state: 'linked',
      events: ['request_opened', 'step_failed'],
      chat: { telegramUserId: 4242, chatId: 4242, username: 'alice_tg' },
      linkCodePending: false,
    });
  });

  test('reasons and the expiry of a waiting code come along when the server sends them', () => {
    const profile = parseTelegramProfile({
      ...unlinkedWire,
      bot_reason: 'Telegram rejected the bot token (401)',
      state: 'error',
      reason: 'Telegram rejected the bot token (401): set a new bot token',
      link_code_pending: true,
      link_code_expires_at: '2026-11-14T10:15:00Z',
    });
    expect(profile.botReason).toBe('Telegram rejected the bot token (401)');
    expect(profile.reason).toBe('Telegram rejected the bot token (401): set a new bot token');
    expect(profile.linkCodePending).toBe(true);
    expect(profile.linkCodeExpiresAt).toBe('2026-11-14T10:15:00Z');
  });

  test('a chat without a known username has none', () => {
    const profile = parseTelegramProfile({
      ...linkedWire,
      chat: { telegram_user_id: 4242, chat_id: 4242 },
    });
    expect(profile.chat).toEqual({ telegramUserId: 4242, chatId: 4242 });
  });

  test('a malformed profile fails loudly instead of showing a wrong state', () => {
    for (const broken of [
      null,
      [],
      { ...unlinkedWire, state: '' },
      { ...unlinkedWire, state: 7 },
      { ...unlinkedWire, own_bot: 'yes' },
      { ...unlinkedWire, bot_username: null },
      { ...unlinkedWire, events: 'request_opened' },
      { ...unlinkedWire, events: [1] },
      { ...unlinkedWire, link_code_pending: undefined },
      { ...linkedWire, chat: { telegram_user_id: '4242', chat_id: 4242 } },
      { ...linkedWire, chat: { telegram_user_id: 4242, chat_id: 1.5 } },
      { ...linkedWire, chat: 'alice' },
    ]) {
      expect(() => parseTelegramProfile(broken)).toThrow('Invalid telegram profile response');
    }
  });
});

describe('link code from the wire', () => {
  const code = 'Zk3vQx2b8m1nP0aR7sT4uV6wX9yZ5cD3eF8gH1iJ2kL';

  test('the code, its expiry, the bot and the deep link', () => {
    expect(
      parseTelegramLinkCode({
        code,
        expires_at: '2026-11-14T10:15:00Z',
        bot_username: 'brood_bot',
        url: `https://t.me/brood_bot?start=${code}`,
      })
    ).toEqual({
      code,
      expiresAt: '2026-11-14T10:15:00Z',
      botUsername: 'brood_bot',
      url: `https://t.me/brood_bot?start=${code}`,
    });
  });

  test('a bot without a known name gives a code without a link', () => {
    const parsed = parseTelegramLinkCode({
      code,
      expires_at: '2026-11-14T10:15:00Z',
      bot_username: '',
    });
    expect(parsed).toEqual({ code, expiresAt: '2026-11-14T10:15:00Z', botUsername: '' });
  });

  test('only a t.me link becomes a link; anything else is dropped, not followed', () => {
    for (const url of [
      'javascript:alert(1)',
      'http://t.me/brood_bot',
      'https://evil.example/t.me/',
    ]) {
      const parsed = parseTelegramLinkCode({
        code,
        expires_at: '2026-11-14T10:15:00Z',
        bot_username: 'brood_bot',
        url,
      });
      expect(parsed.url).toBeUndefined();
    }
  });

  test('a code without its expiry is refused', () => {
    expect(() => parseTelegramLinkCode({ code, bot_username: 'brood_bot' })).toThrow(
      'Invalid telegram link code response'
    );
    expect(() =>
      parseTelegramLinkCode({ code: '', expires_at: '2026-11-14T10:15:00Z', bot_username: '' })
    ).toThrow('Invalid telegram link code response');
  });
});

describe('requests', () => {
  const originalFetch = globalThis.fetch;
  const documentProperty = Object.getOwnPropertyDescriptor(globalThis, 'document');
  afterEach(() => {
    globalThis.fetch = originalFetch;
    if (documentProperty) Object.defineProperty(globalThis, 'document', documentProperty);
    else Reflect.deleteProperty(globalThis, 'document');
  });

  const calls: Array<{
    url: string;
    method: string;
    headers: Record<string, string>;
    body?: string;
  }> = [];
  const serve = (reply: (method: string, url: string) => Response) => {
    calls.length = 0;
    Object.defineProperty(globalThis, 'document', {
      configurable: true,
      value: { cookie: 'csrf=telegram-test' },
    });
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      calls.push({
        url,
        method,
        headers: (init?.headers ?? {}) as Record<string, string>,
        body: init?.body as string | undefined,
      });
      return reply(method, url);
    }) as typeof fetch;
  };

  test('the profile is read with the session, without a body', async () => {
    serve(() => Response.json(linkedWire));
    const { data: profile } = await telegramApi.profile();
    expect(profile.state).toBe('linked');
    expect(calls).toEqual([
      { url: '/api/me/telegram', method: 'GET', headers: {}, body: undefined },
    ]);
  });

  test('every result comes with the server clock of its reply, when the reply has one', async () => {
    const date = 'Sat, 14 Nov 2026 10:00:00 GMT';
    serve((method) =>
      method === 'POST'
        ? Response.json(
            { code: 'c0de', expires_at: '2026-11-14T10:15:00Z', bot_username: 'brood_bot' },
            { status: 201, headers: { Date: date } }
          )
        : Response.json(unlinkedWire, { headers: { Date: date } })
    );
    expect((await telegramApi.profile()).date).toBe(date);
    expect((await telegramApi.setToken(TOKEN)).date).toBe(date);
    expect((await telegramApi.remove()).date).toBe(date);
    expect((await telegramApi.issueLinkCode()).date).toBe(date);
    serve(() => Response.json(unlinkedWire));
    expect((await telegramApi.profile()).date).toBeNull();
  });

  test('saving sends exactly {"token"} with CSRF, and the result does not carry the token', async () => {
    // Сервер, нарушивший контракт, вернул токен эхом в лишних полях: в
    // профиль панели не попадает ни одно из них.
    serve(() => Response.json({ ...linkedWire, state: 'unlinked', chat: null, ...echoed }));
    const result = await telegramApi.setToken(TOKEN);
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe('/api/me/telegram');
    expect(calls[0].method).toBe('PUT');
    expect(calls[0].headers['X-CSRFToken']).toBe('telegram-test');
    expect(JSON.parse(calls[0].body ?? '')).toEqual({ token: TOKEN });
    expect(Object.keys(result.data).sort()).toEqual(PROFILE_KEYS);
    expect(JSON.stringify(result)).not.toContain(SECRET);
    expect(result.data.botUsername).toBe('alice_dev_bot');
  });

  test('a read keeps only the known profile fields, even when the server echoes the token', async () => {
    serve(() =>
      Response.json({
        ...linkedWire,
        bot_reason: 'stopped',
        state: 'error',
        reason: 'Telegram rejected the bot token (401)',
        link_code_pending: true,
        link_code_expires_at: '2026-11-14T10:15:00Z',
        chat: { ...linkedWire.chat, token: TOKEN },
        ...echoed,
      })
    );
    const result = await telegramApi.profile();
    expect(Object.keys(result.data).sort()).toEqual(
      [...PROFILE_KEYS, 'botReason', 'linkCodeExpiresAt', 'reason'].sort()
    );
    expect(Object.keys(result.data.chat ?? {}).sort()).toEqual([
      'chatId',
      'telegramUserId',
      'username',
    ]);
    expect(JSON.stringify(result)).not.toContain(SECRET);
  });

  test('removal and a new link code are writes with CSRF', async () => {
    serve((method) =>
      method === 'POST'
        ? Response.json(
            {
              code: 'Zk3vQx2b8m1nP0aR7sT4uV6wX9yZ5cD3eF8gH1iJ2kL',
              expires_at: '2026-11-14T10:15:00Z',
              bot_username: 'brood_bot',
              url: 'https://t.me/brood_bot?start=Zk3vQx2b8m1nP0aR7sT4uV6wX9yZ5cD3eF8gH1iJ2kL',
            },
            { status: 201 }
          )
        : Response.json(unlinkedWire)
    );
    expect((await telegramApi.remove()).data.state).toBe('unlinked');
    expect((await telegramApi.issueLinkCode()).data.botUsername).toBe('brood_bot');
    expect(calls.map(({ method, url }) => `${method} ${url}`)).toEqual([
      'DELETE /api/me/telegram',
      'POST /api/me/telegram/link',
    ]);
    for (const call of calls) {
      expect(call.headers['X-CSRFToken']).toBe('telegram-test');
      expect(call.body).toBeUndefined();
    }
  });

  test('a refusal carries the public reason of the server', async () => {
    serve(() =>
      Response.json(
        { detail: 'Telegram rejected the bot token', reason: 'Telegram rejected the bot token' },
        { status: 422 }
      )
    );
    const refusal = await telegramApi.setToken(TOKEN).catch((error: unknown) => error);
    expect(refusal).toBeInstanceOf(BroodApiError);
    expect((refusal as BroodApiError).status).toBe(422);
    expect((refusal as BroodApiError).message).toBe('Telegram rejected the bot token');
  });
});
