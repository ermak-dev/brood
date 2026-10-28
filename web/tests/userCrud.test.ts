// FEATURE: User CRUD uses one editable property form.
// SPEC: Role and blocked status are properties, persisted by one atomic request.
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

test('reads, updates and deletes the selected user through the CRUD contract', async () => {
  const calls: Array<{ url: string; method?: string; body?: BodyInit | null }> = [];
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { cookie: 'csrf=test' } });
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, method: init?.method, body: init?.body });
    return init?.method === 'GET'
      ? Response.json({ username: 'a/b' })
      : new Response(null, { status: 204 });
  }) as typeof fetch;
  await accessApi.user('a/b');
  await accessApi.updateUser('a/b', { display_name: 'Name', is_admin: false, disabled: true });
  await accessApi.deleteUser('a/b');
  expect(calls.map(({ url, method }) => [url, method])).toEqual([
    ['/api/admin/users/a%2Fb', 'GET'],
    ['/api/admin/users/a%2Fb', 'PUT'],
    ['/api/admin/users/a%2Fb', 'DELETE'],
  ]);
  expect(JSON.parse(String(calls[1].body))).toEqual({
    display_name: 'Name',
    is_admin: false,
    disabled: true,
  });
});

test('uses populated forms instead of separate role and block commands', () => {
  const page = readFileSync(
    new URL('../src/features/access/UsersPage.tsx', import.meta.url),
    'utf8'
  );
  expect(page).toContain('accessApi.updateUser');
  expect(page).toContain('accessApi.deleteUser');
  expect(page).toContain('initialValues=');
  expect(page).toContain("name: 'disabled'");
  expect(page).toContain("name: 'is_admin'");
  expect(page).not.toContain('accessApi.setRole');
  expect(page).not.toContain('accessApi.setDisabled');
});
