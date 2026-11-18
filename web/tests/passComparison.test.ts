import { expect, test } from 'bun:test';
import { parsePassComparison, passComparisonApi } from '@/services/brood/passComparison';
import { swarmPassComparisonPath } from '@/features/swarms/routes';
import { passComparison } from './fixtures/passComparison';

test('comparison parser preserves missing actual results separately from retained reports', () => {
  const parsed = parsePassComparison(passComparison());
  expect(parsed.steps[0].reference?.outcome).toBeUndefined();
  expect(parsed.steps[0].reference?.reportedOutcome).toBe('success');
  expect(parsed.steps[0].candidate?.outcome).toBe('fail');
  expect(parsed.steps[0].durationDeltaMillis).toBeUndefined();
  expect(parsed).toEqual(passComparison());
});

test('comparison parser refuses ambiguous positions, counts, scope and invented duration', () => {
  const row = passComparison().steps[0];
  for (const changes of [
    { version: 2 },
    { alignment: 'semantic-equivalence' },
    { reference: { swarmId: '', pass: 1 } },
    { candidate: { swarmId: 'candidate', pass: 0 } },
    { sameGoal: 'yes' },
    { steps: [] },
    { stepCount: { reference: 1, candidate: 1, delta: 1 } },
    { childTaskCount: { reference: 0, candidate: 0, delta: -1 } },
    { steps: [{ ...row, position: 0 }] },
    { steps: [{ ...row, node: 'missing' }] },
    { steps: [{ ...row, outcome: 'approved' }] },
    { steps: [{ ...row, durationDeltaMillis: 0 }] },
    { steps: [{ ...row, reference: { ...row.reference, durationMillis: -1 } }] },
    { steps: [{ ...row, reference: { ...row.reference, sequence: 2 } }] },
  ])
    expect(() => parsePassComparison(passComparison(changes))).toThrow();
});

test('comparison transport escapes both scopes and refuses a stale foreign pair', async () => {
  const original = globalThis.fetch;
  const calls: Array<{ url: string; method?: string }> = [];
  let reply = passComparison({
    reference: { swarmId: 'ref/one', pass: 1 },
    candidate: { swarmId: 'other & swarm', pass: 10 },
  });
  globalThis.fetch = (async (input, init) => {
    calls.push({ url: String(input), method: init?.method });
    return new Response(JSON.stringify(reply), { status: 200 });
  }) as typeof fetch;
  try {
    await passComparisonApi.get('ref/one', 1, 'other & swarm', 10);
    expect(calls).toEqual([
      {
        url: '/api/swarms/ref%2Fone/passes/1/compare?swarm=other+%26+swarm&pass=10',
        method: 'GET',
      },
    ]);
    expect(passComparisonApi.downloadUrl('ref/one', 1, 'other & swarm', 10)).toBe(
      '/api/swarms/ref%2Fone/passes/1/compare?swarm=other+%26+swarm&pass=10&download=1'
    );
    expect(swarmPassComparisonPath('ref/one', 1)).toBe('/swarms/ref%2Fone/passes/1/compare');
    reply = passComparison({ candidate: { swarmId: 'foreign', pass: 10 } });
    await expect(passComparisonApi.get('ref/one', 1, 'other & swarm', 10)).rejects.toThrow();
    const count = calls.length;
    await expect(passComparisonApi.get('ref/one', 0, 'other', 10)).rejects.toThrow();
    await expect(passComparisonApi.get('ref/one', 1, '', 10)).rejects.toThrow();
    expect(calls).toHaveLength(count);
  } finally {
    globalThis.fetch = original;
  }
});
