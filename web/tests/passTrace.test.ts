import { expect, test } from 'bun:test';
import { parsePassTrace, passTraceApi } from '@/services/brood/passTrace';
import { swarmPassPath } from '@/features/swarms/routes';
import { passTrace, traceDelivery } from './fixtures/passTrace';

test('trace parser preserves reports and unknown receipt separately from actual outcome', () => {
  const value = passTrace();
  value.steps[0] = { ...value.steps[0], instruction: traceDelivery() };
  const parsed = parsePassTrace(value);
  expect(parsed.steps[0].outcome).toBeUndefined();
  expect(parsed.steps[0].reportedResult?.outcome).toBe('success');
  expect(parsed.steps[0].instruction?.attempts[0].resultRetained).toBe(false);
  expect(parsed.steps[0].instruction?.attempts[0].outcome).toBeUndefined();
  expect(parsed).toEqual(value);
});

test('trace parser rejects malformed identity, cross-pass material and implicit actor ownership', () => {
  for (const change of [
    { version: 2 },
    { swarmId: '' },
    { pass: 0 },
    { pass: 1.5 },
    { source: { origin: 'https://git.example.org/project', commit: '' } },
    { graph: {} },
    { current: 'true' },
    { steps: [passTrace().steps[0], passTrace().steps[0]] },
    { steps: [{ ...passTrace().steps[0], sequence: 0 }] },
    { steps: [{ ...passTrace().steps[0], sequence: 1.5 }] },
    { requests: [{ ...passTrace().requests[0], swarmId: 'foreign' }] },
    { requests: [{ ...passTrace().requests[0], sequence: 1 }] },
    { requests: [{ ...passTrace().requests[0], createdDuring: { pass: 9, observedSequence: 1 } }] },
    {
      requests: [{ ...passTrace().requests[0], createdDuring: { pass: 1, observedSequence: 99 } }],
    },
    { gaps: [{ kind: 'instruction_not_retained', sequence: '1' }] },
    { current: false, execution: 'running' },
  ]) {
    expect(() => parsePassTrace(passTrace(change))).toThrow();
  }
  expect(
    parsePassTrace(passTrace({ current: false, execution: undefined })).execution
  ).toBeUndefined();
});

test('trace parser never turns a missing receipt or a probe into a new successful send', () => {
  const original = traceDelivery().attempts[0];
  for (const attempt of [
    { ...original, outcome: 'not_sent' },
    { ...original, resultRetained: true },
    { ...original, number: 0 },
    { ...original, mode: 'unknown' },
    { ...original, mode: 'probe', resultRetained: true, outcome: 'delivered' },
    { ...original, mode: 'probe', resultRetained: true, outcome: 'not_sent' },
  ]) {
    const value = passTrace();
    value.steps[0] = { ...value.steps[0], instruction: traceDelivery({ attempts: [attempt] }) };
    expect(() => parsePassTrace(value)).toThrow();
  }
});

test('trace transport uses exact escaped swarm/pass and rejects stale foreign replies', async () => {
  const original = globalThis.fetch;
  const calls: Array<{ url: string; method?: string }> = [];
  let reply = passTrace({
    swarmId: 'swarm/other',
    pass: 12,
    steps: [{ ...passTrace().steps[0], sequence: 12 }],
    requests: [],
    gaps: [],
  });
  globalThis.fetch = (async (input, init) => {
    calls.push({ url: String(input), method: init?.method });
    return new Response(JSON.stringify(reply), { status: 200 });
  }) as typeof fetch;
  try {
    expect((await passTraceApi.get('swarm/other', 12)).pass).toBe(12);
    expect(calls).toEqual([{ url: '/api/swarms/swarm%2Fother/passes/12', method: 'GET' }]);
    expect(passTraceApi.downloadUrl('swarm/other', 12)).toBe(
      '/api/swarms/swarm%2Fother/passes/12?download=1'
    );
    expect(swarmPassPath('swarm/other', 12)).toBe('/swarms/swarm%2Fother/passes/12');
    reply = passTrace({ swarmId: 'foreign' });
    await expect(passTraceApi.get('swarm/other', 12)).rejects.toThrow();
    const count = calls.length;
    await expect(passTraceApi.get('swarm/other', 0)).rejects.toThrow();
    await expect(passTraceApi.get('', 1)).rejects.toThrow();
    expect(calls).toHaveLength(count);
  } finally {
    globalThis.fetch = original;
  }
});
