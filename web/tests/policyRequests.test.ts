// Feature: revision-bound policy confirmation.
// Spec: only an open, supported revision can submit yes/no; no graph route or actor retry.
// Implementation: preserve the wire revision and send it with the selected confirmation.
// Result: implemented. Status: done.
import { expect, test } from 'bun:test';
import { parseSwarmRequest } from '@/services/brood/requests';
import { answerBody, deliveryRetryable, requestForm } from '@/features/swarms/requestState';

const wire = (over: Record<string, unknown> = {}) => ({
  id: 'policy:candidate',
  swarmId: 'swarm',
  origin: 'policy',
  kind: 'confirm',
  revision: 1,
  sequence: 1,
  nodeId: 'build',
  state: 'open',
  question: 'Approve automatic transition?',
  context: '{"candidate_id":"candidate","human_rules":["storage"]}',
  options: [],
  createdAt: '2026-11-16T08:00:00Z',
  ...over,
});

test('policy wire preserves explicit safe revision without inferring a missing one', () => {
  expect(parseSwarmRequest(wire()).revision).toBe(1);
  for (const revision of [undefined, null, 0, -1, 1.5, '1', Number.MAX_SAFE_INTEGER + 1]) {
    expect(() => parseSwarmRequest(wire({ revision }))).toThrow();
  }
});

test('policy confirmation has yes/no and exact revision but no graph outcome', () => {
  const request = parseSwarmRequest(wire());
  expect(requestForm(request)).toMatchObject({ route: false, confirm: true, text: 'none' });
  expect(answerBody(request, { choice: null, text: '' })).toBeNull();
  expect(answerBody(request, { choice: 'maybe', text: '' })).toBeNull();
  expect(answerBody(request, { choice: 'yes', text: '' })).toEqual({
    response: 'yes',
    revision: 1,
  });
  expect(answerBody(request, { choice: 'no', text: '' })).toEqual({ response: 'no', revision: 1 });
  expect(requestForm(parseSwarmRequest(wire({ revision: 2 })))).toBeNull();
  expect(requestForm(parseSwarmRequest(wire({ kind: 'text' })))).toBeNull();
});

test('answered policy is history, not an actor delivery or a new confirmation', () => {
  const request = parseSwarmRequest(wire({ state: 'answered', revision: 2, response: 'yes' }));
  expect(deliveryRetryable(request)).toBe(false);
  expect(answerBody(request, { choice: 'yes', text: '' })).toBeNull();
});
