import { afterEach, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { accessApi } from '@/services/brood/access';

const originalFetch = globalThis.fetch;
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

test('permissions can list members and change global administrator rights via Go', async () => {
  const calls: Array<{ url: string; init?: RequestInit }> = [];
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=test' },
  });
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return init?.method === 'GET' ? Response.json([]) : new Response(null, { status: 204 });
  }) as typeof fetch;
  await accessApi.members('project/one');
  await accessApi.setRole('alice/bob', true);
  expect(calls.map((call) => call.url)).toEqual([
    '/api/projects/project%2Fone/members',
    '/api/admin/users/alice%2Fbob/role',
  ]);
  expect(calls[1].init?.body).toBe('{"is_admin":true}');
});

test('member listing and grant/revoke actions are exposed in the same panel', () => {
  const permissions = readFileSync(
    new URL('../src/features/access/PermissionsPage.tsx', import.meta.url),
    'utf8'
  );
  const users = readFileSync(
    new URL('../src/features/access/UsersPage.tsx', import.meta.url),
    'utf8'
  );
  expect(permissions).toContain('accessApi.members');
  expect(permissions).toContain('EntityTable');
  expect(users).toContain('accessApi.setRole');
});
