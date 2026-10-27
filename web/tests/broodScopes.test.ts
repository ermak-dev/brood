import { afterEach, expect, test } from 'bun:test';
import { actorsApi, policyApi, scopesApi } from '@/services/brood/substrate';

const originalFetch = globalThis.fetch;
const calls: Array<{ url: string; init?: RequestInit }> = [];
afterEach(() => {
  globalThis.fetch = originalFetch;
  calls.splice(0);
});

test('Substrate scope lists only accessible swarms and keeps project IDs separate', async () => {
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    if (url === '/api/projects') return Response.json([{ id: 'project-1', name: 'Project' }]);
    if (url === '/api/projects/project-1/swarms')
      return Response.json([
        { id: 'swarm-1', project_id: 'project-1', name: 'My agents', state: 'active' },
      ]);
    throw new Error(`unexpected ${url}`);
  }) as typeof fetch;
  const scopes = await scopesApi.list();
  expect(scopes).toHaveLength(1);
  expect(scopes[0].metadata.name).toBe('swarm-1');
  expect(scopes[0].label).toContain('My agents');
  expect(scopes[0].label).toContain('Project');
});

test('terminal links go through the authorized Go actor-view redirect', () => {
  expect(actorsApi.terminal('swarm/one', 'actor name')).toBe(
    '/api/swarms/swarm%2Fone/proxy/actors/actor%20name/'
  );
});

test('actor and policy reads use authorized swarm paths, never project/atespace paths', async () => {
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return Response.json({ results: [] });
  }) as typeof fetch;
  await actorsApi.list('swarm/one');
  await policyApi.get('swarm/one');
  expect(calls.map((call) => call.url)).toEqual([
    '/api/swarms/swarm%2Fone/actors',
    '/api/swarms/swarm%2Fone/policy',
  ]);
});
