import { afterEach, expect, test } from 'bun:test';
import { accessApi } from '../src/services/brood/access';

const originalFetch = globalThis.fetch;
const documentProperty = Object.getOwnPropertyDescriptor(globalThis, 'document');
const calls: Array<{ url: string; init?: RequestInit }> = [];
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (documentProperty) Object.defineProperty(globalThis, 'document', documentProperty);
  else Reflect.deleteProperty(globalThis, 'document');
  calls.length = 0;
});

// FEATURE: Typed shard capacity and local swarm ACL HTTP clients (#53).
// SPEC: Real Go paths, exact nullable decimal strings and username/role bodies;
// identifiers are escaped and no native policy or arbitrary principal is invented.
test('uses project detail/capacity and swarm member endpoints without losing zero or int64 precision', async () => {
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { cookie: 'csrf=test' } });
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return new Response(JSON.stringify([]), { status: 200 });
  }) as typeof fetch;
  const capacity = {
    worker_selector: { shard: 'one' },
    max_swarms: null,
    max_actors: '0',
    max_cpu_millicores: '2000',
    max_memory_bytes: '9223372036854775807',
  };
  await accessApi.project('project/id');
  await accessApi.setCapacity('project/id', capacity);
  await accessApi.swarmMembers('swarm/id');
  await accessApi.setSwarmMember('swarm/id', 'a/b', 'viewer');
  await accessApi.removeSwarmMember('swarm/id', 'a/b');
  expect(calls.map((call) => [call.init?.method, call.url])).toEqual([
    ['GET', '/api/projects/project%2Fid'],
    ['PUT', '/api/projects/project%2Fid/capacity'],
    ['GET', '/api/swarms/swarm%2Fid/members'],
    ['POST', '/api/swarms/swarm%2Fid/members'],
    ['DELETE', '/api/swarms/swarm%2Fid/members/a%2Fb'],
  ]);
  expect(JSON.parse(String(calls[1].init?.body))).toEqual(capacity);
  expect(JSON.parse(String(calls[3].init?.body))).toEqual({ username: 'a/b', role: 'viewer' });
});
