// FEATURE: WorkerPool lists use the Kubernetes items envelope.
// SPEC: Existing pools must appear; malformed responses must not look empty.
import { afterEach, expect, test } from 'bun:test';
import { workerPoolsApi } from '@/services/brood/substrate';

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

function reply(value: unknown) {
  globalThis.fetch = (async () => Response.json(value)) as typeof fetch;
}

test('reads existing pools from items, not the ateapi results envelope', async () => {
  const pool = { metadata: { name: 'development' }, spec: { replicas: 2 } };
  reply({ items: [pool] });
  expect(await workerPoolsApi.list()).toEqual([pool]);
});

test('accepts an empty or null Kubernetes collection', async () => {
  reply({ items: [] });
  expect(await workerPoolsApi.list()).toEqual([]);
  reply({ items: null });
  expect(await workerPoolsApi.list()).toEqual([]);
});

test('rejects a malformed collection rather than hiding all pools', async () => {
  for (const invalid of [{ results: [] }, { items: 'wrong' }, null]) {
    reply(invalid);
    await expect(workerPoolsApi.list()).rejects.toThrow('invalid WorkerPool list');
  }
});
