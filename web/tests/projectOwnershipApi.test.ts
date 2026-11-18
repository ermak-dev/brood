// FEATURE: Ownership API использует явный CAS, точный scope и не повторяет запись.
// SPEC: Чужой response и malformed metadata не дают прав; 404 не маскируется default owner.
import { afterEach, expect, test } from 'bun:test';
import { projectOwnershipApi } from '@/services/brood/projectOwnership';

const fetchBefore = globalThis.fetch;
const documentBefore = Object.getOwnPropertyDescriptor(globalThis, 'document');
const calls: Array<{ url: string; init?: RequestInit }> = [];
afterEach(() => {
  globalThis.fetch = fetchBefore;
  if (documentBefore) Object.defineProperty(globalThis, 'document', documentBefore);
  else Reflect.deleteProperty(globalThis, 'document');
  calls.length = 0;
});
const owner = {
  projectId: 'project/id',
  viewerUid: 'alice-id',
  revision: 1,
  ownerUid: 'alice-id',
  ownerUsername: 'alice',
  updatedBy: 'admin-id',
  updatedAt: '2026-11-18T10:00:00Z',
};
function fixture(response: (call: number) => Response) {
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=test' },
  });
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return response(calls.length);
  }) as typeof fetch;
}
const json = (value: unknown) => new Response(JSON.stringify(value), { status: 200 });

test('ownership get, assign and revoke use exact escaped scope and revision', async () => {
  fixture((call) =>
    json(
      call === 1
        ? owner
        : call === 2
          ? { ...owner, revision: 2 }
          : {
              ...owner,
              revision: 3,
              ownerUid: '',
              ownerUsername: '',
            }
    )
  );
  expect(await projectOwnershipApi.get('project/id')).toEqual(owner);
  await projectOwnershipApi.set('project/id', 1, 'alice');
  const revoked = await projectOwnershipApi.set('project/id', 2, '');
  expect(revoked.ownerUid).toBe('');
  expect(calls.map((call) => [call.init?.method, call.url])).toEqual([
    ['GET', '/api/projects/project%2Fid/owner'],
    ['PUT', '/api/projects/project%2Fid/owner'],
    ['PUT', '/api/projects/project%2Fid/owner'],
  ]);
  expect(JSON.parse(String(calls[1].init?.body))).toEqual({ revision: 1, username: 'alice' });
  expect(JSON.parse(String(calls[2].init?.body))).toEqual({ revision: 2, username: '' });
  expect(new Headers(calls[1].init?.headers).get('X-CSRFToken')).toBe('test');
});

test('foreign ownership response and unretained CAS fail closed', async () => {
  fixture(() => json({ ...owner, projectId: 'foreign' }));
  await expect(projectOwnershipApi.get('project/id')).rejects.toThrow();
  fixture(() => json(owner));
  await expect(projectOwnershipApi.set('project/id', 1, 'alice')).rejects.toThrow();
});

test('ownership errors are not retried or converted to unassigned ownership', async () => {
  fixture(() => new Response(JSON.stringify({ detail: 'project not found' }), { status: 404 }));
  await expect(projectOwnershipApi.get('project/id')).rejects.toThrow('project not found');
  expect(calls.length).toBe(1);
  fixture(
    () =>
      new Response(JSON.stringify({ detail: 'project owner revision changed' }), { status: 409 })
  );
  await expect(projectOwnershipApi.set('project/id', 1, 'alice')).rejects.toThrow(
    'project owner revision changed'
  );
  expect(calls.length).toBe(2);
});
