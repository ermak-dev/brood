// FEATURE: Команды сессии и workspace из панели через /api/commands (#151).
// SPEC: Команда — POST /api/commands/{tool} с {swarm_id, arguments} и CSRF;
// неизвестный исход (заголовок X-Brood-Command-Outcome или outcome в теле)
// помечается явно и не превращается ни в успех, ни в обычный отказ. История и
// usage нормализуются при чтении; repository проекта и remotes установки не
// выдают ничего, кроме origin и признака учётных данных.
import { afterEach, beforeEach, expect, test } from 'bun:test';
import {
  CommandError,
  commandsApi,
  historyEntry,
  sessionApi,
} from '@/services/brood/commands';
import { accessApi } from '@/services/brood/access';
import { BroodApiError } from '@/services/brood/http';
import { performManagedAction } from '@/features/substrate/actorOperations';

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: unknown;
}
const calls: Call[] = [];
const originalFetch = globalThis.fetch;
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
let reply: () => Response = () => Response.json({ text: '', outcome: 'completed' });

beforeEach(() => {
  calls.length = 0;
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=command-test' },
  });
  globalThis.fetch = (async (url: string, options: RequestInit = {}) => {
    calls.push({
      url,
      method: options.method ?? 'GET',
      headers: (options.headers ?? {}) as Record<string, string>,
      body: options.body === undefined ? undefined : JSON.parse(options.body as string),
    });
    return reply();
  }) as typeof fetch;
});
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

test('a command posts the tool, the swarm scope and its arguments with CSRF', async () => {
  reply = () => Response.json({ text: 'done', data: { ok: 1 }, outcome: 'completed' });
  const result = await commandsApi.run('tail', 'swarm/a', { name: 'coding', lines: 40 });
  expect(calls).toEqual([
    {
      url: '/api/commands/tail',
      method: 'POST',
      headers: { 'X-CSRFToken': 'command-test', 'Content-Type': 'application/json' },
      body: { swarm_id: 'swarm/a', arguments: { name: 'coding', lines: 40 } },
    },
  ]);
  expect(result).toEqual({ text: 'done', data: { ok: 1 }, outcome: 'completed', unconfirmed: false });
});

test('workspace mutations always send the explicit confirmation the server requires', async () => {
  for (const tool of ['restart', 'recycle', 'wipe'] as const) {
    await sessionApi[tool]('swarm', 'coding');
    expect(calls.at(-1)).toMatchObject({
      url: `/api/commands/${tool}`,
      body: { swarm_id: 'swarm', arguments: { name: 'coding', confirm: true } },
    });
  }
});

test('an unknown outcome is flagged from the header or the body, never as plain success', async () => {
  reply = () =>
    Response.json(
      { detail: 'workspace operation op-1 outcome is unconfirmed; reservation retained' },
      { status: 409, headers: { 'X-Brood-Command-Outcome': 'unconfirmed' } }
    );
  const failure = await sessionApi.restart('swarm', 'coding').catch((error: unknown) => error);
  expect(failure).toBeInstanceOf(CommandError);
  expect(failure).toBeInstanceOf(BroodApiError);
  expect(failure).toMatchObject({
    status: 409,
    unconfirmed: true,
    message: 'workspace operation op-1 outcome is unconfirmed; reservation retained',
  });

  reply = () => Response.json({ detail: 'release active coordination first' }, { status: 409 });
  expect(await sessionApi.recycle('swarm', 'coding').catch((e: unknown) => e)).toMatchObject({
    unconfirmed: false,
    message: 'release active coordination first',
  });

  reply = () => Response.json({ text: 'submitted; delivery unconfirmed', outcome: 'unconfirmed' });
  expect((await sessionApi.send('swarm', 'coding', 'hello', 0)).unconfirmed).toBe(true);
  reply = () => Response.json({ text: 'x', outcome: 'unknown' });
  expect((await sessionApi.slash('swarm', 'coding', '/status')).unconfirmed).toBe(true);
  reply = () => Response.json({ text: 'input submitted', outcome: 'submitted' });
  expect((await sessionApi.slash('swarm', 'coding', '/status')).unconfirmed).toBe(false);
});

test('send and slash post exactly the catalog arguments', async () => {
  reply = () => Response.json({ text: 'delivered', outcome: 'completed' });
  await sessionApi.send('swarm', 'coding', 'Run the tests', 30);
  expect(calls.at(-1)?.body).toEqual({
    swarm_id: 'swarm',
    arguments: { to: 'coding', message: 'Run the tests', wait_seconds: 30 },
  });
  await sessionApi.slash('swarm', 'coding', '/compact');
  expect(calls.at(-1)?.body).toEqual({
    swarm_id: 'swarm',
    arguments: { name: 'coding', command: '/compact' },
  });
});

test('tail returns the raw screen text, even when it happens to be JSON', async () => {
  reply = () => Response.json({ text: '42\n', data: 42, outcome: 'complete' });
  expect(await sessionApi.tail('swarm', 'coding', { lines: 100, grep: '' })).toBe('42\n');
  expect(calls.at(-1)?.body).toEqual({
    swarm_id: 'swarm',
    arguments: { name: 'coding', lines: 100 },
  });
  await sessionApi.tail('swarm', 'coding', { lines: 40, grep: 'FAIL' });
  expect(calls.at(-1)?.body).toEqual({
    swarm_id: 'swarm',
    arguments: { name: 'coding', lines: 40, grep: 'FAIL' },
  });
});

test('history pages keep the opaque cursor and normalize transcript records', async () => {
  reply = () =>
    Response.json({
      text: '{}',
      outcome: 'complete',
      data: {
        epoch: 'e1',
        session_id: 's1',
        cursor: 'c2',
        reset: false,
        has_more: true,
        partial: false,
        records: [
          {
            type: 'user',
            timestamp: '2026-11-01T10:00:00Z',
            message: { role: 'user', content: 'Run the tests' },
          },
          {
            type: 'assistant',
            timestamp: '2026-11-01T10:00:05Z',
            message: {
              role: 'assistant',
              content: [
                { type: 'thinking', thinking: 'private chain' },
                { type: 'text', text: 'Running them now.' },
                { type: 'tool_use', name: 'Bash', input: { command: 'go test ./...' } },
              ],
            },
          },
        ],
      },
    });
  const page = await sessionApi.history('swarm', 'coding', { cursor: 'c1', limit: 50 });
  expect(calls.at(-1)?.body).toEqual({
    swarm_id: 'swarm',
    arguments: { name: 'coding', cursor: 'c1', limit: 50 },
  });
  expect(page).toEqual({
    epoch: 'e1',
    cursor: 'c2',
    reset: false,
    hasMore: true,
    partial: false,
    entries: [
      { kind: 'user', time: '2026-11-01T10:00:00Z', text: 'Run the tests' },
      { kind: 'assistant', time: '2026-11-01T10:00:05Z', text: 'Running them now.\n→ Bash' },
    ],
  });
  await sessionApi.history('swarm', 'coding', { cursor: '', limit: 50 });
  expect(calls.at(-1)?.body).toEqual({
    swarm_id: 'swarm',
    arguments: { name: 'coding', limit: 50 },
  });

  reply = () => Response.json({ text: 'not json', outcome: 'complete' });
  await expect(sessionApi.history('swarm', 'coding', { cursor: '', limit: 50 })).rejects.toThrow(
    'invalid session history'
  );
});

test('history records become short readable entries without private reasoning', () => {
  expect(
    historyEntry({
      type: 'user',
      message: { content: [{ type: 'tool_result', content: [{ type: 'text', text: 'ok' }] }] },
    })
  ).toEqual({ kind: 'tool', text: 'ok' });
  expect(
    historyEntry({ type: 'assistant', message: { content: [{ type: 'thinking', thinking: 'x' }] } })
  ).toBeNull();
  expect(historyEntry({ type: 'summary', summary: 'Fixed the build' })).toEqual({
    kind: 'summary',
    text: 'Fixed the build',
  });
  expect(historyEntry({ type: 'brood_history_notice', reason: 'record_too_large' })).toEqual({
    kind: 'notice',
    text: 'record_too_large',
  });
  expect(historyEntry({ type: 'file-history-snapshot' })).toEqual({
    kind: 'other',
    text: 'file-history-snapshot',
  });
  expect(historyEntry('garbage')).toEqual({ kind: 'notice', text: 'invalid_record' });
  const long = historyEntry({ type: 'user', message: { content: 'a'.repeat(5000) } });
  expect(long?.text.length).toBeLessThanOrEqual(2001);
  expect(long?.text.endsWith('…')).toBe(true);
});

test('usage keeps exact totals, the partial flag and days newest first', async () => {
  reply = () =>
    Response.json({
      text: '{}',
      outcome: 'complete',
      data: {
        days: 7,
        input: 1200,
        output: 300,
        cache_write: 50,
        cache_read: 9000,
        attribution: 'unknown',
        partial: true,
        files: 2,
        scope: 'this guest project; bounded transcript scan',
        per_day: { '2026-10-30': [1, 2, 3, 4], '2026-11-01': [10, 20, 30, 40] },
      },
    });
  const usage = await sessionApi.stat('swarm', 'coding', 7);
  expect(calls.at(-1)?.body).toEqual({ swarm_id: 'swarm', arguments: { name: 'coding', days: 7 } });
  expect(usage).toEqual({
    days: 7,
    input: 1200,
    output: 300,
    cacheWrite: 50,
    cacheRead: 9000,
    partial: true,
    files: 2,
    scope: 'this guest project; bounded transcript scan',
    perDay: [
      { day: '2026-11-01', input: 10, output: 20, cacheWrite: 30, cacheRead: 40 },
      { day: '2026-10-30', input: 1, output: 2, cacheWrite: 3, cacheRead: 4 },
    ],
  });
  reply = () => Response.json({ text: '{}', outcome: 'complete', data: { days: 'x' } });
  await expect(sessionApi.stat('swarm', 'coding', 7)).rejects.toThrow('invalid usage report');
});

test('project repository and installation remotes expose origin and credential presence only', async () => {
  reply = () => Response.json({ origin: 'https://git.example.com/team/repo' });
  expect(await accessApi.repository('p/1')).toBe('https://git.example.com/team/repo');
  expect(calls.at(-1)).toMatchObject({ url: '/api/projects/p%2F1/repository', method: 'GET' });

  reply = () =>
    Response.json({
      items: [
        { origin: 'https://git.example.com/team/repo', credentialConfigured: true, token: 'leak' },
        { origin: 'https://public.example.com/x', credentialConfigured: false },
      ],
    });
  expect(await accessApi.gitRemotes()).toEqual([
    { origin: 'https://git.example.com/team/repo', credentialConfigured: true },
    { origin: 'https://public.example.com/x', credentialConfigured: false },
  ]);
  expect(calls.at(-1)).toMatchObject({ url: '/api/git-remotes', method: 'GET' });

  reply = () => new Response(null, { status: 204 });
  await accessApi.setRepository('p/1', '');
  expect(calls.at(-1)).toMatchObject({
    url: '/api/projects/p%2F1/repository',
    method: 'PUT',
    body: { origin: '' },
    headers: { 'X-CSRFToken': 'command-test' },
  });

  reply = () => Response.json({ origin: 5 });
  await expect(accessApi.repository('p')).rejects.toThrow('invalid project repository');
  reply = () => Response.json({ items: [{ origin: 'x' }] });
  await expect(accessApi.gitRemotes()).rejects.toThrow('invalid Git remote list');
});

test('lifecycle uses the actor API, session and workspace actions use commands', async () => {
  reply = () => Response.json({ metadata: { name: 'coding' } });
  const expected = {
    start: ['/api/swarms/s%2F1/actors/c%2F1', 'POST', { action: 'resume' }],
    stop: ['/api/swarms/s%2F1/actors/c%2F1', 'POST', { action: 'stop' }],
    reset: ['/api/swarms/s%2F1/actors/c%2F1', 'POST', { action: 'reset' }],
    restart: ['/api/commands/restart', 'POST', { swarm_id: 's/1', arguments: { name: 'c/1', confirm: true } }],
    recycle: ['/api/commands/recycle', 'POST', { swarm_id: 's/1', arguments: { name: 'c/1', confirm: true } }],
    wipe: ['/api/commands/wipe', 'POST', { swarm_id: 's/1', arguments: { name: 'c/1', confirm: true } }],
  } as const;
  for (const [id, [url, method, body]] of Object.entries(expected)) {
    expect(await performManagedAction('s/1', 'c/1', id as keyof typeof expected)).toEqual({
      unconfirmed: false,
    });
    expect(calls.at(-1)).toMatchObject({ url, method, body });
  }
  reply = () => new Response(null, { status: 202 });
  await performManagedAction('s/1', 'c/1', 'delete');
  expect(calls.at(-1)).toMatchObject({ url: '/api/swarms/s%2F1/actors/c%2F1', method: 'DELETE' });
  expect(calls).toHaveLength(7);

  reply = () => Response.json({ text: '', outcome: 'unknown' });
  expect(await performManagedAction('s', 'c', 'restart')).toEqual({ unconfirmed: true });
  // A terminal is a link, never a mutation.
  await expect(performManagedAction('s', 'c', 'terminal')).rejects.toThrow('not a mutation');
});
