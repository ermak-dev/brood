// FEATURE: Управление пользователями, проектами, членством и роями через Go API.
// SPEC: Точные пути и тела, экранирование сегментов, без выдуманных GET members/retry.
// IMPLEMENTATION: Типизированный слой поверх общего same-origin request.
// RESULT: IMPLEMENTED — проверены контракты Go, пустые списки и отказы.
// STATUS: DONE
import { afterEach, expect, test } from 'bun:test';

const originalFetch = globalThis.fetch;
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
const calls: Array<{ url: string; init?: RequestInit }> = [];
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
  calls.length = 0;
});

function mockResponse(body: unknown = []) {
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=test' },
  });
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return new Response(JSON.stringify(body), { status: 200 });
  }) as typeof fetch;
}

test('uses Go user contracts, including new_password and boolean is_admin', async () => {
  mockResponse();
  const { accessApi } = await import('../src/services/brood/access');
  await accessApi.createUser({
    username: 'a/b',
    password: 'secret',
    display_name: 'A',
    is_admin: false,
  });
  await accessApi.setDisabled('a/b', true);
  await accessApi.setDisabled('a/b', false);
  await accessApi.resetPassword('a/b', 'new secret');
  expect(calls.map((c) => c.url)).toEqual([
    '/api/admin/users',
    '/api/admin/users/a%2Fb/disable',
    '/api/admin/users/a%2Fb/enable',
    '/api/admin/users/a%2Fb/password-reset',
  ]);
  expect(JSON.parse(String(calls[0].init?.body)).is_admin).toBe(false);
  expect(JSON.parse(String(calls[3].init?.body))).toEqual({ new_password: 'new secret' });
});

test('separates project IDs from swarm IDs and membership usernames', async () => {
  mockResponse();
  const { accessApi } = await import('../src/services/brood/access');
  await accessApi.projects();
  await accessApi.createProject('Team');
  await accessApi.swarms('project/id');
  await accessApi.createSwarm('project/id', 'Swarm');
  await accessApi.addMember('project/id', 'a/b');
  await accessApi.removeMember('project/id', 'a/b');
  await accessApi.transferSwarm('swarm/id', 'a/b');
  await accessApi.deleteSwarm('swarm/id');
  await accessApi.deleteProject('project/id');
  expect(calls.map((c) => [c.init?.method, c.url])).toEqual([
    ['GET', '/api/projects'],
    ['POST', '/api/projects'],
    ['GET', '/api/projects/project%2Fid/swarms'],
    ['POST', '/api/projects/project%2Fid/swarms'],
    ['POST', '/api/projects/project%2Fid/members'],
    ['DELETE', '/api/projects/project%2Fid/members/a%2Fb'],
    ['POST', '/api/swarms/swarm%2Fid/transfer'],
    ['DELETE', '/api/swarms/swarm%2Fid'],
    ['DELETE', '/api/projects/project%2Fid'],
  ]);
  expect(JSON.parse(String(calls[4].init?.body))).toEqual({ username: 'a/b' });
});

test('preserves empty lists and server refusal reasons', async () => {
  mockResponse([]);
  const { accessApi } = await import('../src/services/brood/access');
  expect(await accessApi.projects()).toEqual([]);
  expect(await accessApi.users()).toEqual([]);
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ detail: 'new owner must be a project member' }), {
      status: 403,
    })) as typeof fetch;
  await expect(accessApi.transferSwarm('id', 'outsider')).rejects.toThrow(
    'new owner must be a project member'
  );
});
