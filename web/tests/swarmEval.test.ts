// FEATURE: Isolated, explicitly pinned eval bindings (#293).
// SPEC: Only 404 means unbound; bind sends four exact fields, start no selection;
// malformed or cross-swarm projections fail closed, Unicode reason stays verbatim.
// IMPLEMENTATION: Typed eval endpoints, strict scope/selection decoding and explicit model validation.
// RESULT: IMPLEMENTED — request and decoder regressions pass without normalizing server text.
// STATUS: DONE
import { afterEach, expect, test } from 'bun:test';
import { evalApi, parseSwarmEval, validEvalSelection } from '@/services/brood/eval';
import en from '@/i18n/locales/en.json';
import ru from '@/i18n/locales/ru.json';
import zhCN from '@/i18n/locales/zh-CN.json';
import zhTW from '@/i18n/locales/zh-TW.json';
import vi from '@/i18n/locales/vi.json';
import ko from '@/i18n/locales/ko.json';

export const binding = () => ({
  swarmId: 'candidate',
  referenceSwarmId: 'reference',
  referencePass: 3,
  definition: { name: 'review', version: 2, revision: 'revision' },
  model: 'explicit-model',
  source: { origin: 'https://example.test/repo', commit: 'a'.repeat(40) },
  goal: '<script>not html</script>\n﻿​',
  nonProduction: true,
  execution: 'idle',
  replay: null,
});
const original = globalThis.fetch;
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
afterEach(() => {
  globalThis.fetch = original;
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

test('eval labels and isolation instructions exist in all six locales without fallback', () => {
  const keys = Object.keys(en.swarm_eval).sort();
  for (const locale of [en, ru, zhCN, zhTW, vi, ko]) {
    expect(Object.keys(locale.swarm_eval).sort()).toEqual(keys);
    for (const value of Object.values(locale.swarm_eval)) expect(value.length).toBeGreaterThan(0);
  }
});

test('eval replay decodes the actual boolean interruption field, never a child count', () => {
  for (const interruptedChildren of [false, true]) {
    const value = {
      ...binding(),
      replay: { requestId: 'request', state: 'stopped', reason: 'Unmatched question', interruptedChildren },
    };
    expect(parseSwarmEval(value)).toEqual(value);
  }
  for (const interruptedChildren of [0, 1, 2, -1, 'true', null, undefined]) {
    expect(() => parseSwarmEval({
      ...binding(),
      replay: { requestId: 'request', state: 'stopped', reason: 'Unmatched question', interruptedChildren },
    })).toThrow();
  }
});

test('strict eval projection preserves goal and durable replay reason without trim', () => {
  const value = {
    ...binding(),
    replay: { requestId: 'request', state: 'stopped', reason: '﻿​', interruptedChildren: true },
  };
  expect(parseSwarmEval(value)).toEqual(value);
  for (const changes of [
    { nonProduction: false },
    { referencePass: 0 },
    { referencePass: 1.5 },
    { model: '' },
    { definition: { name: 'review', version: 0, revision: 'r' } },
    { goal: null },
    { source: { origin: 'o' } },
    { execution: '' },
    { replay: { requestId: 'r', state: 'running', reason: '', interruptedChildren: false } },
    { replay: { requestId: 'r', state: 'stopped', reason: 'x', interruptedChildren: -1 } },
    { replay: undefined },
    { replay: { requestId: 'r', state: ['applied'], reason: '', interruptedChildren: false } },
  ])
    expect(() => parseSwarmEval({ ...value, ...changes })).toThrow();
});

test('selection requires explicit positive pass and printable ASCII model, never trims it', () => {
  const selection = {
    referenceSwarmId: 'reference',
    referencePass: 3,
    definition: 'review',
    model: 'explicit-model',
  };
  expect(validEvalSelection(selection)).toBe(true);
  for (const model of ['', ' model', 'model ', 'a b', '﻿model', '模型', 'x'.repeat(257)])
    expect(validEvalSelection({ ...selection, model })).toBe(false);
  for (const model of [undefined, null, 123, ['model']])
    expect(validEvalSelection({ ...selection, model } as unknown as typeof selection)).toBe(false);
  for (const referencePass of [0, -1, 1.5, NaN])
    expect(validEvalSelection({ ...selection, referencePass })).toBe(false);
});

test('eval API uses encoded scope, exact bind body and bodyless pinned start', async () => {
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=test' },
  });
  const calls: { url: string; method?: string; body?: BodyInit | null }[] = [];
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, method: init?.method, body: init?.body });
    return Response.json(
      url.endsWith('/start')
        ? { swarmId: 'candidate/a', execution: 'running' }
        : { ...binding(), swarmId: 'candidate/a' }
    );
  }) as typeof fetch;
  const selection = {
    referenceSwarmId: 'reference',
    referencePass: 3,
    definition: 'review',
    model: 'explicit-model',
  };
  await evalApi.bind('candidate/a', selection);
  await evalApi.get('candidate/a');
  await evalApi.start('candidate/a');
  expect(calls).toEqual([
    { url: '/api/swarms/candidate%2Fa/eval', method: 'POST', body: JSON.stringify(selection) },
    { url: '/api/swarms/candidate%2Fa/eval', method: 'GET', body: undefined },
    { url: '/api/swarms/candidate%2Fa/eval/start', method: 'POST', body: undefined },
  ]);
  globalThis.fetch = (async () => Response.json(binding())) as typeof fetch;
  await expect(evalApi.get('other')).rejects.toThrow();
});

test('bind rejects mismatched selection echoes and invalid selection never sends a request', async () => {
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=test' },
  });
  let calls = 0;
  globalThis.fetch = (async () => {
    calls++;
    return Response.json(binding());
  }) as typeof fetch;
  const selection = {
    referenceSwarmId: 'reference',
    referencePass: 3,
    definition: 'review',
    model: 'another-model',
  };
  await expect(evalApi.bind('candidate', selection)).rejects.toThrow();
  const before = calls;
  await expect(evalApi.bind('candidate', { ...selection, model: '' })).rejects.toThrow();
  expect(calls).toBe(before);
});

test('eval start validates the normal execution projection including optional snapshot fields', async () => {
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=test' },
  });
  for (const changes of [
    { swarmId: 'other' },
    { execution: 'unknown' },
    { source: { origin: 'o', commit: 42 } },
    { definition: { name: 'd', version: 0, revision: 'r' } },
    { goal: null },
    { step: { sequence: 0, nodeId: 'n', state: 'running' } },
  ]) {
    globalThis.fetch = (async () =>
      Response.json({ swarmId: 'candidate', execution: 'running', ...changes })) as typeof fetch;
    await expect(evalApi.start('candidate')).rejects.toThrow();
  }
});

test('only a 404 is an ordinary swarm; other refusals retain the server reason', async () => {
  for (const status of [404, 403, 409, 500]) {
    globalThis.fetch = (async () =>
      Response.json({ detail: 'server reason﻿​' }, { status })) as typeof fetch;
    if (status === 404) expect(await evalApi.get('candidate')).toBeNull();
    else await expect(evalApi.get('candidate')).rejects.toThrow('server reason﻿​');
  }
});
