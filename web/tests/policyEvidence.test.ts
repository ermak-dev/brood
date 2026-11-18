import { expect, test } from 'bun:test';
import { parsePolicyCandidate, policyEvidenceApi } from '@/services/brood/policyEvidence';

import { candidate } from './fixtures/policyEvidence';

test('policy evidence preserves every retained path and rule without interpreting text', () => {
  expect(parsePolicyCandidate(candidate())).toEqual(candidate());
});

test('policy evidence rejects missing or malformed trust and identity fields', () => {
  for (const change of [
    { swarmId: undefined },
    { id: '' },
    { sequence: 1.5 },
    { policyRevision: '3' },
    { evidence: { ...candidate().evidence, complete: undefined } },
    { evidence: { ...candidate().evidence, paths: ['good', 17] } },
    { decision: { ...candidate().decision, matches: [{ ruleId: 'storage' }] } },
    { decision: { ...candidate().decision, unknown: undefined } },
    { rules: [{ id: 'storage', paths: [null], require: 'human' }] },
    { version: 2 },
  ]) {
    expect(() => parsePolicyCandidate(candidate(change))).toThrow();
  }
});

test('policy evidence transport scopes the exact candidate and refuses mismatched replies', async () => {
  const original = globalThis.fetch;
  const calls: Array<{ url: string; method: string | undefined }> = [];
  let reply = candidate({ id: 'candidate/2', swarmId: 'swarm/other' });
  globalThis.fetch = (async (input, init) => {
    calls.push({ url: String(input), method: init?.method });
    return new Response(JSON.stringify(reply), { status: 200 });
  }) as typeof fetch;
  try {
    expect(await policyEvidenceApi.detail('swarm/other', 'policy:candidate/2')).toEqual(reply);
    expect(calls).toEqual([
      { url: '/api/swarms/swarm%2Fother/requests/policy%3Acandidate%2F2/policy', method: 'GET' },
    ]);
    reply = candidate({ id: 'wrong', swarmId: 'swarm/other' });
    await expect(policyEvidenceApi.detail('swarm/other', 'policy:candidate/2')).rejects.toThrow();
    reply = candidate({ id: 'candidate/2', swarmId: 'foreign' });
    await expect(policyEvidenceApi.detail('swarm/other', 'policy:candidate/2')).rejects.toThrow();
    const count = calls.length;
    await expect(policyEvidenceApi.detail('swarm', 'actor-request')).rejects.toThrow();
    expect(calls).toHaveLength(count);
  } finally {
    globalThis.fetch = original;
  }
});

test('terminal evidence keeps an explicit no-successor result distinct from malformed targets', () => {
  const terminal = candidate({ terminal: true, target: '' });
  expect(parsePolicyCandidate(terminal)).toEqual(terminal);
  for (const change of [
    { terminal: undefined },
    { terminal: false },
    { terminal: null },
    { terminal: 'true' },
    { target: 'invented-exit' },
    { targetKind: 'actor' },
    { targetKind: 'human' },
    { initial: true },
    { boundary: 'entry' },
  ]) {
    expect(() => parsePolicyCandidate({ ...terminal, ...change })).toThrow();
  }
  expect(parsePolicyCandidate(candidate({ terminal: false }))).toEqual(
    candidate({ terminal: false })
  );
});

test('initial evidence keeps absence explicit rather than inventing an artifact', () => {
  const initial = candidate({
    boundary: 'entry',
    initial: true,
    target: 'build',
    targetKind: 'actor',
    actionsKnown: true,
    actions: ['landing'],
    evidence: { id: '', known: false, complete: false, artifact: '', producerUid: '', paths: [] },
  });
  expect(parsePolicyCandidate(initial)).toEqual(initial);
});
