// FEATURE: Access management chooses real users; native policy uses catalog choices.
// SPEC: Never accept free-form principals or map a local username to a native identity.
import { afterEach, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { policyApi } from '@/services/brood/substrate';
const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');
const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
});

test('default access policy edits local project membership and retains a separate native page', () => {
  const routes = source('router/MainRoutes.tsx');
  expect(routes).toContain("path: '/policy', element: <PermissionsPage />");
  expect(routes).toContain("path: '/substrate-policy', element: <PolicyPage />");
  expect(source('features/access/PermissionsPage.tsx')).toContain('AccessMatrix');
  expect(source('features/access/PermissionsPage.tsx')).toContain('memberCandidates');
  const native = source('features/substrate/PolicyPage.tsx');
  expect(native).toContain('PrincipalPicker');
  expect(native).not.toContain('ChipsInput');
  expect(native).not.toContain('placeholder="user:');
});

test('reads native identity choices from the administrator API', async () => {
  const calls: string[] = [];
  globalThis.fetch = (async (url: string) => {
    calls.push(url);
    return Response.json([{ id: 'user:system:serviceaccount:brood:django', label: 'Server' }]);
  }) as typeof fetch;
  expect(await policyApi.identities()).toEqual([
    { id: 'user:system:serviceaccount:brood:django', label: 'Server' },
  ]);
  expect(calls).toEqual(['/api/identities']);
});

test('choice catalog preserves existing policy members and removes duplicates', async () => {
  const { principalOptions } = await import('../src/features/substrate/principalOptions');
  expect(
    principalOptions(
      [{ id: 'user:server', label: 'Server' }],
      [
        { role: 'owner', members: ['user:server', 'user:legacy'] },
        { role: 'viewer', members: ['user:legacy'] },
      ]
    )
  ).toEqual([
    { value: 'user:server', label: 'Server' },
    { value: 'user:legacy', label: 'legacy' },
  ]);
  expect(principalOptions(null, [])).toEqual([]);
});
