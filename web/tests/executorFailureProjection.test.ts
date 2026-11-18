// FEATURE: Registered executor failures settle one slot without synthetic actor results.
// SPEC: Strict keyed membership/count/open checks; preserve legacy and pre-registration failure.
// IMPLEMENTATION: Separate keyed facts and exact slot formula, cross-checked with trace jobs.
// RESULT: IMPLEMENTED — decoder RED→GREEN and real Go API browser acceptance.
// STATUS: DONE
import { expect, test } from 'bun:test';
import { parseChildGroup, parseChildGroupPage } from '@/services/brood/children';
import { parsePassTrace } from '@/services/brood/passTrace';
import { childGroup, childTask } from './fixtures/swarmChildren';
import { ephemeralJob, passTrace } from './fixtures/passTrace';

const fact = {
  intentId: 'job-1',
  childId: 'child-1',
  requestId: 'independent-1',
  reason: 'Explicit local rejection.\nNo report.',
  failedAt: '2026-11-17T00:06:00Z',
};
const raw = () => ({
  ...childGroup(),
  executorFailed: 1,
  open: 1,
  children: [{ ...childTask({ role: '', delivery: '' }), executorFailure: fact }],
});

test('registered executor failure is preserved and counted once', () => {
  expect(parseChildGroup(raw())).toEqual(raw());
  expect(parseChildGroupPage({ items: [raw()], nextBefore: 0 }).items[0]).toHaveProperty(
    'executorFailed',
    1
  );
  expect(parseChildGroup(childGroup())).toEqual(childGroup());
  expect(parseChildGroup({ ...raw(), state: 'aborted', open: 0 }).open).toBe(0);
});

// BUG: ECMAScript trim отвергал FEFF, допустимый для Go strings.TrimSpace.
// HYPOTHESIS: Child decoder проверял reason иначе, чем server и trace decoder.
// SOLUTION: Проверяется Unicode White_Space, без ECMAScript FEFF trimming.
// RESULT: FIXED — 2604 web tests и реальный Go API/browser с FEFF проходят.
// STATUS: FIXED
test('executor failure reasons preserve Go whitespace semantics across projections', () => {
  for (const reason of ['﻿', '​', '﻿\n']) {
    const decision = { ...fact, reason };
    const group = {
      ...raw(),
      children: [{ ...raw().children[0], executorFailure: decision }],
    };
    expect(parseChildGroup(group).children[0].executorFailure?.reason).toBe(reason);
    const value = passTrace();
    const job = ephemeralJob({
      id: fact.intentId,
      requestId: fact.requestId,
      childId: fact.childId,
      executorFailure: decision,
    });
    const trace = {
      ...value,
      steps: [{ ...value.steps[0], children: { ...group, parentSequence: 1 }, ephemeralJobs: [job] }],
    };
    expect(parsePassTrace(trace).steps[0].ephemeralJobs[0].executorFailure?.reason).toBe(reason);
  }
  for (const reason of [' ', '\t\n', '\u0085', ' ', ' ']) {
    expect(() =>
      parseChildGroup({
        ...raw(),
        children: [{ ...raw().children[0], executorFailure: { ...fact, reason } }],
      })
    ).toThrow('Invalid child history response');
  }
});

test('executor failures cannot mask inconsistent counts, membership or actor results', () => {
  for (const count of [undefined, null, -1, 0, 2, 1.5, '1']) {
    expect(() => parseChildGroup({ ...raw(), executorFailed: count })).toThrow(
      'Invalid child history response'
    );
  }
  for (const over of [
    { childId: 'foreign' },
    { requestId: 'foreign' },
    { intentId: '' },
    { reason: ' ' },
    { failedAt: '' },
  ]) {
    expect(() =>
      parseChildGroup({
        ...raw(),
        children: [{ ...raw().children[0], executorFailure: { ...fact, ...over } }],
      })
    ).toThrow('Invalid child history response');
  }
  for (const over of [
    { state: 'completed' },
    { outcome: 'failed' },
    { report: 'fabricated report' },
    { role: 'review' },
    { delivery: 'queued' },
  ]) {
    expect(() =>
      parseChildGroup({ ...raw(), children: [{ ...raw().children[0], ...over }] })
    ).toThrow('Invalid child history response');
  }
  expect(() => parseChildGroup({ ...raw(), open: 2 })).toThrow('Invalid child history response');
  expect(() =>
    parseChildGroupPage({ items: [{ ...raw(), executorFailed: 2 }], nextBefore: 0 })
  ).toThrow('Invalid child history response');
});

test('trace failure facts agree with child membership and cannot be duplicated', () => {
  const value = passTrace();
  const job = ephemeralJob({
    id: fact.intentId,
    requestId: fact.requestId,
    childId: fact.childId,
    executorFailure: fact,
  });
  const trace = (jobs: unknown[]) => ({
    ...value,
    steps: [{ ...value.steps[0], children: { ...raw(), parentSequence: 1 }, ephemeralJobs: jobs }],
  });
  expect(parsePassTrace(trace([job])).steps[0].children?.executorFailed).toBe(1);
  for (const jobs of [
    [],
    [job, job],
    [{ ...job, executorFailure: { ...fact, reason: 'different decision' } }],
    [{ ...job, executorFailure: undefined }],
  ]) {
    expect(() => parsePassTrace(trace(jobs))).toThrow('Invalid pass trace response');
  }
});

test('trace retains keyed executor decision separately and rejects impossible job facts', () => {
  const trace = (job: Record<string, unknown>) => {
    const value = passTrace();
    return { ...value, steps: [{ ...value.steps[0], ephemeralJobs: [job] }] };
  };
  const job = ephemeralJob({
    id: fact.intentId,
    requestId: fact.requestId,
    childId: fact.childId,
    executorFailure: fact,
  });
  expect(parsePassTrace(trace(job)).steps[0].ephemeralJobs[0]).toEqual(job);
  for (const over of [
    { childId: 'other' },
    { id: 'other' },
    { requestId: 'other' },
    { childId: undefined },
    { executorFailure: { ...fact, reason: ' ' } },
    { executorFailure: null },
    { failure: { reason: 'pre-registration', failedAt: fact.failedAt } },
    {
      resultRetention: {
        childId: fact.childId,
        childRevision: 2,
        outcome: 'failed',
        parentOutcome: 'failed',
        retainedAt: fact.failedAt,
      },
    },
  ]) {
    expect(() => parsePassTrace(trace({ ...job, ...over }))).toThrow('Invalid pass trace response');
  }
});
