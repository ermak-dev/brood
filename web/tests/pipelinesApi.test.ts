// FEATURE: Pipelines API contract for the engine that does not exist yet (#169; engine #168).
// SPEC: Typed calls to the Go server's own paths, IDs escaped, answer bodies exact.
import { afterEach, expect, test } from 'bun:test';
import { pipelinesApi } from '../src/services/brood/pipelines';

const originalFetch = globalThis.fetch;
const documentProperty = Object.getOwnPropertyDescriptor(globalThis, 'document');
const calls: Array<{ url: string; init?: RequestInit }> = [];
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (documentProperty) Object.defineProperty(globalThis, 'document', documentProperty);
  else Reflect.deleteProperty(globalThis, 'document');
  calls.length = 0;
});

function reply(body: unknown, status = 200) {
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return new Response(JSON.stringify(body), { status });
  }) as typeof fetch;
}

test('run and gate calls go to the pipeline-run paths with escaped IDs and exact bodies', async () => {
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=test' },
  });
  const run = {
    id: 'run/one',
    name: 'release',
    status: 'waiting_human',
    startedAt: '2026-11-05T09:00:00Z',
    nodes: [],
  };
  reply([run]);
  expect(await pipelinesApi.listRuns('swarm/one')).toEqual([run]);
  reply(run);
  await pipelinesApi.getRun('run/one');
  reply(run);
  await pipelinesApi.answerGate('run/one', 'node/a', 'ship it', 3);
  reply({ svg: '<svg></svg>' });
  await pipelinesApi.getRunGraph('run/one');
  expect(calls.map((call) => [call.init?.method, call.url])).toEqual([
    ['GET', '/api/swarms/swarm%2Fone/pipeline-runs'],
    ['GET', '/api/pipeline-runs/run%2Fone'],
    ['POST', '/api/pipeline-runs/run%2Fone/nodes/node%2Fa/answer'],
    ['GET', '/api/pipeline-runs/run%2Fone/graph'],
  ]);
  expect(JSON.parse(String(calls[2].init?.body))).toEqual({ answer: 'ship it', visitSequence: 3 });
  expect(calls[2].init?.headers).toMatchObject({ 'X-CSRFToken': 'test' });
});

test('a refused answer keeps the server reason as an error, not a silent success', async () => {
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=test' },
  });
  reply({ detail: 'gate already answered' }, 409);
  await expect(pipelinesApi.answerGate('r', 'n', 'late', 1)).rejects.toMatchObject({
    status: 409,
    message: 'gate already answered',
  });
});
