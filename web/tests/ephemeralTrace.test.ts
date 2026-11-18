// FEATURE: История ephemeral jobs дополняет trace, не заменяя инструкции и отчёты.
// SPEC: Legacy => []; malformed present => отказ всего trace. Intent до allocation;
// reservation не квитанция, proof не отчёт, inventory absence не deletion,
// result retention не разрешение landing/cleanup. Только публичные DTO и UID/inc.
// IMPLEMENTATION: Строгий decoder и отдельные факты с disclosure исходной задачи;
// инструкции и отчёты остаются в прежней истории без поиска актора по имени.
// RESULT: IMPLEMENTED — behavioral decoder/render tests проходят.
// STATUS: DONE
import { afterAll, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { PassTraceContent } from '@/features/swarms/SwarmPassTrace';
import { parsePassTrace } from '@/services/brood/passTrace';
import en from '@/i18n/locales/en.json';
import { allocatedJob, ephemeralJob, ephemeralTrace } from './fixtures/passTrace';
import { installTestDOM } from './helpers/dom';
import { childGroup, childTask } from './fixtures/swarmChildren';

const restore = installTestDOM();
afterAll(restore);
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

// FEATURE: Registered server failures render separately from actor reports.
// SPEC: Exact reason is escaped, physical holds and result absence stay distinct.
test('registered executor failure is labelled as a server decision, not an actor result', async () => {
  const fact = {
    intentId: 'job-registered',
    childId: 'child-1',
    requestId: 'ephemeral-request',
    reason: 'Rejected <script>not executable</script>\nNo result.',
    failedAt: '2026-11-17T00:06:00Z',
  };
  const raw = ephemeralTrace([
    ephemeralJob({ id: fact.intentId, childId: fact.childId, executorFailure: fact }),
  ]);
  const trace = parsePassTrace({
    ...raw,
    steps: [
      {
        ...raw.steps[0],
        children: childGroup({
          parentSequence: 1,
          executorFailed: 1,
          open: 1,
          children: [
            {
              ...childTask({ requestId: fact.requestId, role: '', delivery: '' }),
              executorFailure: fact,
            },
          ],
        }),
      },
    ],
  });
  const { renderToStaticMarkup } = await import('react-dom/server');
  const html = renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(PassTraceContent, { trace }))
  );
  expect(html).toContain('Registered executor server failure');
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        createElement(I18nextProvider, { i18n }, createElement(PassTraceContent, { trace }))
      )
    );
    const details = [...container.querySelectorAll('details')].find(
      (node) => node.querySelector('summary')?.textContent === 'Server failure reason'
    );
    expect(details).toBeDefined();
    await act(async () => {
      details!.open = true;
      details!.dispatchEvent(new window.Event('toggle'));
    });
    expect(details!.textContent).toContain(fact.reason);
    expect(details!.querySelector('script')).toBeNull();
  } finally {
    await act(async () => root.unmount());
    container.remove();
  }
  expect(html).toContain(
    'This decision is not an actor report, result seal, or cleanup permission. Physical resources may still be held.'
  );
  expect(html).not.toContain('<script>');
});

test('decoder defaults only missing jobs and preserves FIFO independent facts verbatim', () => {
  const legacy = ephemeralTrace();
  delete (legacy.steps[0] as Record<string, unknown>).ephemeralJobs;
  expect(parsePassTrace(legacy).steps[0].ephemeralJobs).toEqual([]);
  expect(parsePassTrace(ephemeralTrace([])).steps[0].ephemeralJobs).toEqual([]);
  const jobs = [ephemeralJob(), allocatedJob()];
  expect(parsePassTrace(ephemeralTrace(jobs)).steps[0].ephemeralJobs).toEqual(jobs);
  for (const state of ['queued', 'legacy']) {
    expect(() =>
      parsePassTrace(
        ephemeralTrace([
          allocatedJob({
            createAuthorization: { state, createdAt: '2026-11-17T00:01:00Z' },
          }),
        ])
      )
    ).not.toThrow();
  }
  for (const state of ['pending', 'unconfirmed']) {
    expect(() =>
      parsePassTrace(
        ephemeralTrace([
          allocatedJob({
            childId: undefined,
            resultRetention: undefined,
            attestation: { state, createdAt: 'then', updatedAt: 'later' },
          }),
        ])
      )
    ).not.toThrow();
  }
});

test('malformed jobs fail the whole trace rather than disappearing or becoming legacy', () => {
  for (const jobs of [null, {}, 'bad', [null], [ephemeralJob(), null]]) {
    expect(() => parsePassTrace(ephemeralTrace(jobs))).toThrow('Invalid pass trace response');
  }
  for (const over of [
    { id: '' },
    { requestId: null },
    { task: {} },
    { poolUid: 7 },
    { queuedAt: null },
    { source: null },
    { source: { origin: '', commit: 'c'.repeat(40) } },
    { source: { origin: 'origin', commit: 4 } },
    { allocation: null },
    { allocation: { actorUid: 'uid', actorIncarnation: 0, allocatedAt: 'then' } },
    { allocation: { actorUid: 'uid', actorIncarnation: 1.5, allocatedAt: 'then' } },
    { createAuthorization: { state: 'success', createdAt: 'then' } },
    { createAuthorization: { state: 'reserved', createdAt: 'then' } },
    { createAuthorization: { state: 'queued', createdAt: 'then', reservedAt: 'later' } },
    { attestation: null },
    { attestation: { state: 'success', createdAt: 'then', updatedAt: 'later' } },
    { attestation: { state: 'attested', createdAt: 'then', updatedAt: 'later' } },
    {
      attestation: {
        state: 'pending',
        createdAt: 'then',
        updatedAt: 'later',
        verifiedCommit: 'wrong',
      },
    },
    { childId: 42 },
    { inventory: {} },
    { inventoryUnavailable: 'deleted' },
    { inventoryUnavailable: null },
    { resultRetention: null },
    {
      resultRetention: {
        childId: 'child-1',
        childRevision: 0,
        outcome: 'success',
        parentOutcome: 'success',
        retainedAt: 'then',
      },
    },
  ]) {
    expect(() => parsePassTrace(ephemeralTrace([allocatedJob(over)]))).toThrow(
      'Invalid pass trace response'
    );
  }
});

test('job enum fields reject arrays instead of coercing them to valid strings', () => {
  for (const over of [
    { createAuthorization: { state: ['queued'], createdAt: 'then' } },
    { attestation: { state: ['pending'], createdAt: 'then', updatedAt: 'later' } },
    { inventoryUnavailable: ['not_retained'] },
  ]) {
    expect(() => parsePassTrace(ephemeralTrace([allocatedJob(over)]))).toThrow(
      'Invalid pass trace response'
    );
  }
});

async function withTrace(value: unknown, check: (container: HTMLElement) => Promise<void>) {
  const trace = parsePassTrace(value);
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        createElement(I18nextProvider, { i18n }, createElement(PassTraceContent, { trace }))
      )
    );
    await check(container);
  } finally {
    await act(async () => root.unmount());
    container.remove();
  }
}
async function open(details: HTMLDetailsElement) {
  await act(async () => {
    details.open = true;
    details.dispatchEvent(new window.Event('toggle'));
  });
}

test('queued intent is visible before allocation with pinned source and safely disclosed original task', async () => {
  await withTrace(ephemeralTrace([ephemeralJob()]), async (container) => {
    const jobs = container.querySelector('[aria-label="Ephemeral jobs"]');
    expect(jobs).not.toBeNull();
    expect(jobs!.textContent).toContain('job-queued');
    expect(jobs!.textContent).toContain('ephemeral-request');
    expect(jobs!.textContent).toContain('pool-historical');
    expect(jobs!.textContent).toContain('Queued intent');
    expect(jobs!.textContent).toContain('Expected pinned source');
    expect(jobs!.textContent).toContain('https://git.example.org/pinned');
    expect(jobs!.textContent).toContain('c'.repeat(40));
    expect(jobs!.textContent).not.toContain('Allocation');
    expect(jobs!.textContent).not.toContain('Result retention');
    const task = jobs!.querySelector('details')!;
    expect(task.querySelector('pre')).toBeNull();
    await open(task);
    expect(task.querySelector('pre')!.textContent).toBe(ephemeralJob().task);
    expect(jobs!.querySelector('script')).toBeNull();
    expect(container.querySelector('button,form')).toBeNull();
  });
});

test('allocation, reserved authorization, source proof, child and retained result remain separate evidence', async () => {
  await withTrace(ephemeralTrace(), async (container) => {
    const jobs = container.querySelector('[aria-label="Ephemeral jobs"]');
    expect(jobs).not.toBeNull();
    expect(jobs!.textContent).toContain('Allocation');
    expect(jobs!.textContent).toContain('removed-executor / 4');
    expect(jobs!.textContent).toContain(
      'Reserved: creation may have been issued; not a success receipt.'
    );
    expect(jobs!.textContent).toContain('Source proof');
    expect(jobs!.textContent).toContain('Registered child');
    expect(jobs!.textContent).toContain('child-1');
    expect(jobs!.textContent).toContain('Inventory not retained');
    expect(jobs!.textContent).toContain('Historical absence is not proof of deletion.');
    expect(jobs!.textContent).toContain('Result retention');
    expect(jobs!.textContent).toContain('Child revision');
    expect(jobs!.textContent).toContain('Parent outcome');
    expect(jobs!.textContent).toContain('Retained result does not authorize landing or cleanup.');
    expect(jobs!.textContent).not.toContain('Independent completed report.');
    expect(jobs!.textContent).not.toContain('Transport evidence only');
    expect(container.textContent).toContain('Child instruction and delivery: child-1');
    expect(container.textContent).toContain('Original instruction and delivery');
    expect(container.querySelector('button,form')).toBeNull();
    const childHistory = [...container.querySelectorAll('details')].find(
      (node) => node.querySelector('summary')?.textContent === en.swarm_children.title
    )!;
    await open(childHistory);
    expect(childHistory.textContent).toContain('Independent completed report.');
  });
});

test('inventory uses the exact allocated incarnation and labels updatedAt as record time', async () => {
  const inventory = {
    uid: 'removed-executor',
    incarnation: 4,
    currentName: 'retained-name',
    phase: 'ready',
    desired: 'running',
    operation: '',
    stage: '',
    updatedAt: '2026-11-17T00:06:00Z',
  };
  const job = allocatedJob({ inventory, inventoryUnavailable: undefined });
  expect(parsePassTrace(ephemeralTrace([job])).steps[0].ephemeralJobs[0].inventory).toEqual(
    inventory
  );
  await withTrace(ephemeralTrace([job]), async (container) => {
    const jobs = container.querySelector('[aria-label="Ephemeral jobs"]');
    expect(jobs).not.toBeNull();
    expect(jobs!.textContent).toContain('retained-name');
    expect(jobs!.textContent).toContain('Inventory record updated');
    expect(jobs!.textContent).toContain('ready');
    expect(jobs!.textContent).not.toContain('Historical absence');
  });
  for (const over of [{ uid: 'foreign' }, { incarnation: 5 }, { phase: null }, { operation: 1 }]) {
    expect(() =>
      parsePassTrace(
        ephemeralTrace([
          allocatedJob({
            inventory: { ...inventory, ...over },
            inventoryUnavailable: undefined,
          }),
        ])
      )
    ).toThrow('Invalid pass trace response');
  }
});

test('historical inventory mismatch reasons do not name a replacement actor or imply deletion', async () => {
  for (const [reason, label] of [
    ['incarnation_changed', 'Inventory incarnation changed'],
    ['scope_changed', 'Inventory scope changed'],
  ]) {
    await withTrace(
      ephemeralTrace([allocatedJob({ inventoryUnavailable: reason })]),
      async (container) => {
        const jobs = container.querySelector('[aria-label="Ephemeral jobs"]');
        expect(jobs).not.toBeNull();
        expect(jobs!.textContent).toContain(label);
        expect(jobs!.textContent).toContain('Historical absence is not proof of deletion.');
        expect(jobs!.textContent).toContain('removed-executor / 4');
      }
    );
  }
});

test('pre-registration failure rejects malformed facts instead of silently hiding them', () => {
  for (const failure of [
    null,
    {},
    'failed',
    { reason: '', failedAt: 'then' },
    { reason: 'reason', failedAt: null },
  ]) {
    expect(() => parsePassTrace(ephemeralTrace([ephemeralJob({ failure })]))).toThrow(
      'Invalid pass trace response'
    );
  }
  for (const extra of [
    { childId: 'invented' },
    { resultRetention: allocatedJob().resultRetention },
  ]) {
    expect(() =>
      parsePassTrace(
        ephemeralTrace([
          ephemeralJob({ failure: { reason: 'reason', failedAt: 'then' }, ...extra }),
        ])
      )
    ).toThrow('Invalid pass trace response');
  }
});

test('failure reason whitespace follows the server Unicode contract', () => {
  for (const reason of [
    String.fromCodePoint(0xfeff),
    `${String.fromCodePoint(0xfeff)} \t`,
    String.fromCodePoint(0x200b),
  ]) {
    const job = ephemeralJob({ failure: { reason, failedAt: '2026-11-17T00:07:00Z' } });
    expect(parsePassTrace(ephemeralTrace([job])).steps[0].ephemeralJobs[0]).toEqual(job);
  }
  for (const reason of [
    ' ',
    '\t\n',
    ...[0x00a0, 0x2003, 0x2028, 0x3000].map((cp) => String.fromCodePoint(cp)),
  ]) {
    expect(() =>
      parsePassTrace(
        ephemeralTrace([ephemeralJob({ failure: { reason, failedAt: '2026-11-17T00:07:00Z' } })])
      )
    ).toThrow('Invalid pass trace response');
  }
});

test('pre-registration failure is a server decision, not a child report or physical release', async () => {
  const failure = {
    reason: '  Server refused <script>this job</script>\nΩ  ',
    failedAt: '2026-11-17T00:07:00Z',
  };
  const job = ephemeralJob({ failure });
  expect(parsePassTrace(ephemeralTrace([job])).steps[0].ephemeralJobs[0]).toEqual(job);
  await withTrace(ephemeralTrace([job]), async (container) => {
    const jobs = container.querySelector('[aria-label="Ephemeral jobs"]')!;
    expect(jobs.textContent).toContain('Failed before child registration');
    expect(jobs.textContent).toContain('Server decision recorded');
    expect(jobs.textContent).toContain(
      'This is not an independent child report. Physical resources may still be held.'
    );
    expect(jobs.textContent).not.toContain('Result retention');
    const detail = [...jobs.querySelectorAll('details')].find(
      (node) => node.querySelector('summary')?.textContent === 'Server failure reason'
    )!;
    await open(detail);
    expect(detail.querySelector('pre')!.textContent).toBe(failure.reason);
    expect(jobs.querySelector('script,button,form')).toBeNull();
  });
});
