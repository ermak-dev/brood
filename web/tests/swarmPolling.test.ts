// FEATURE: One poll per swarm page (#257).
// SPEC: The swarm page runs one timer while the swarm is active and the tab
// is visible; a hidden tab polls nothing. Each tick reads the execution of a
// live run and the request inbox. The graph is read again only when the
// snapshot revision, the current node or the status of the execution
// changed, and the roles only while the execution is neither running nor
// waiting. Explicit refreshes after an answer, a start or a stop stay.
import { afterAll, afterEach, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { act, createElement, useState } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun } from '@/services/brood/pipelines';
import type { SwarmRequest } from '@/services/brood/requests';
import { useVisibleInterval } from '@/hooks/useInterval';
import { SwarmPage } from '@/features/swarms/SwarmPage';
import {
  shouldRefetchGraph,
  shouldRefetchRoles,
  tickSwarmPolling,
} from '@/features/swarms/useSwarmPolling';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
afterAll(restoreDOM);
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const i18n = createInstance();
await i18n.init({
  lng: 'en',
  resources: { en: { translation: en } },
  interpolation: { escapeValue: false },
});

const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');

const revision = '0123456789abcdef0123456789abcdef';
const other = 'fedcba9876543210fedcba9876543210';

const run = (over: Partial<PipelineRun> = {}): PipelineRun => ({
  id: 'swarm-1',
  name: 'release',
  definition: { name: 'release', version: 1, revision },
  status: 'running',
  swarmId: 'swarm-1',
  swarmName: 'release-crew',
  projectId: 'project-1',
  projectName: 'brood',
  owner: 'alice',
  startedAt: '2026-11-10T08:00:00Z',
  currentNode: 'implement',
  nodes: [
    { id: 'plan', kind: 'actor', label: 'plan', status: 'done', sequence: 1 },
    { id: 'implement', kind: 'actor', label: 'implement', status: 'active', sequence: 2 },
  ],
  ...over,
});

describe('what a tick reads again', () => {
  test('the same execution does not reread the graph', () => {
    expect(shouldRefetchGraph(run(), run())).toBe(false);
    // Details of steps change without the drawing: a report, a moment, an actor name.
    const detailed = run({
      nodes: [
        { id: 'plan', kind: 'actor', label: 'plan', status: 'done', sequence: 1, report: 'ok' },
        {
          id: 'implement',
          kind: 'actor',
          label: 'implement',
          status: 'active',
          sequence: 2,
          actorName: 'builder',
          startedAt: '2026-11-10T08:01:00Z',
        },
      ],
    });
    expect(shouldRefetchGraph(run(), detailed)).toBe(false);
  });

  test('a new snapshot revision, current node or status rereads the graph', () => {
    const snapshot = run({ definition: { name: 'release', version: 2, revision: other } });
    expect(shouldRefetchGraph(run(), snapshot)).toBe(true);
    expect(shouldRefetchGraph(run(), run({ currentNode: 'verify' }))).toBe(true);
    expect(shouldRefetchGraph(run(), run({ status: 'waiting_human' }))).toBe(true);
    expect(shouldRefetchGraph(run(), run({ status: 'stopped' }))).toBe(true);
    // A run without a recorded identity compares the fields it has.
    const bare = run({ definition: undefined });
    expect(shouldRefetchGraph(bare, run({ definition: undefined }))).toBe(false);
    expect(shouldRefetchGraph(bare, run())).toBe(true);
  });

  test('a swarm without a graph has nothing to reread', () => {
    expect(shouldRefetchGraph(run(), null)).toBe(false);
    expect(shouldRefetchGraph(null, null)).toBe(false);
  });

  test('roles are reread only while the execution neither runs nor waits', () => {
    expect(shouldRefetchRoles('running')).toBe(false);
    expect(shouldRefetchRoles('waiting')).toBe(false);
    for (const execution of ['idle', 'succeeded', 'failed', 'stopped']) {
      expect(shouldRefetchRoles(execution)).toBe(true);
    }
  });

  test('the page owns the only timer; the inbox and the roles follow its tick', () => {
    expect(source('features/swarms/SwarmPage.tsx')).toContain('useSwarmPolling(');
    expect(source('features/swarms/SwarmPage.tsx')).not.toContain('useInterval');
    expect(source('features/swarms/useSwarmRequests.ts')).not.toContain('useInterval');
    expect(source('features/swarms/useSwarmRoles.ts')).not.toContain('useInterval');
    expect(source('features/swarms/useSwarmRoles.ts')).toContain('useSwarmTick(');
    expect(source('features/swarms/useSwarmRequests.ts')).toContain('useSwarmTick(');
  });
});

const swarm = (over: Partial<AccessSwarm> = {}): AccessSwarm => ({
  id: 'swarm-1',
  project_id: 'project-1',
  owner_id: 'user-1',
  owner_username: 'alice',
  role: 'owner',
  name: 'release-crew',
  atespace_name: 'brood-1',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-02T09:00:00Z',
  ...over,
});

const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 80">
<g class="node node-actor state-done" data-node-id="plan"><rect x="0" y="0" width="50" height="20"/><text x="25" y="14">plan</text></g>
<g class="node node-actor state-active" data-node-id="implement"><rect x="0" y="30" width="50" height="20"/><text x="25" y="44">implement</text></g>
</svg>`;

const nodeAsk = (): SwarmRequest => ({
  id: 'req-3',
  swarmId: 'swarm-1',
  origin: 'node',
  sequence: 3,
  nodeId: 'implement',
  kind: 'choice',
  question: 'Ship it?',
  context: '',
  options: [
    { label: 'Approve', outcome: 'success' },
    { label: 'Reject', outcome: 'fail' },
  ],
  state: 'open',
  createdAt: '2026-11-10T08:40:06Z',
});

type Reply = { status?: number; body: unknown };
type Call = { method: string; url: string; body: unknown };

/** Go API страницы роя: ответы по методу и адресу, все вызовы записаны. */
function api(routes: Record<string, Reply | ((call: Call) => Reply)>) {
  const calls: Call[] = [];
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    const call = {
      method: init?.method ?? 'GET',
      url,
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
    };
    calls.push(call);
    const route = routes[`${call.method} ${url}`];
    const reply = typeof route === 'function' ? route(call) : route;
    const { status = 200, body } = reply ?? { status: 404, body: { detail: 'not found' } };
    return new Response(JSON.stringify(body), { status });
  }) as typeof fetch;
  return calls;
}

const RUN = '/api/pipeline-runs/swarm-1';
const GRAPH = '/api/pipeline-runs/swarm-1/graph';
const REQUESTS = '/api/swarms/swarm-1/requests?all=1';
const ROLES = '/api/swarms/swarm-1/roles';

/** Сколько раз прочитан каждый адрес страницы с момента отметки. */
function counter(calls: Call[]) {
  let mark = 0;
  const reads = (url: string) =>
    calls.slice(mark).filter((c) => c.method === 'GET' && c.url === url).length;
  return {
    mark: () => {
      mark = calls.length;
    },
    reads: () => ({
      run: reads(RUN),
      graph: reads(GRAPH),
      requests: reads(REQUESTS),
      roles: reads(ROLES),
    }),
  };
}

/** Рой на странице: исполнение и запросы меняются по ходу теста. */
function stand(initial: PipelineRun, requests: SwarmRequest[] = []) {
  const state = { run: initial, requests };
  const execution = () => (state.run.status === 'waiting_human' ? 'waiting' : state.run.status);
  const calls = api({
    'GET /api/swarms/swarm-1': { body: swarm() },
    'GET /api/projects': { body: [{ id: 'project-1', name: 'brood' }] },
    [`GET ${RUN}`]: () => ({ body: state.run }),
    [`GET ${GRAPH}`]: () => ({ body: { svg } }),
    [`GET ${REQUESTS}`]: () => ({ body: { results: state.requests } }),
    [`GET ${ROLES}`]: () => ({
      body: {
        swarmId: 'swarm-1',
        execution: execution(),
        graph: { pinned: true, name: 'release', version: 1, revision },
        roles: [{ role: 'builder', inGraph: true, state: 'unbound' }],
      },
    }),
    'GET /api/swarms/swarm-1/actors': { body: { results: [] } },
    'POST /api/swarms/swarm-1/requests/req-3/answer': () => {
      const answered: SwarmRequest = {
        ...state.requests[0],
        state: 'answered',
        answeredAt: '2026-11-10T09:00:00Z',
        answeredBy: 'alice',
        outcome: 'success',
        optionLabel: 'Approve',
      };
      // The execution has not moved yet: the rereads below owe nothing to a change.
      state.requests = [answered];
      return { body: answered };
    },
    'POST /api/swarms/swarm-1/stop': () => {
      state.run = run({ status: 'stopped' });
      return { body: { swarmId: 'swarm-1', execution: 'stopped' } };
    },
  });
  return { state, calls, ...counter(calls) };
}

const originalFetch = globalThis.fetch;
const documentCookie = Object.getOwnPropertyDescriptor(document, 'cookie');
let mounted: { root: Root; container: HTMLElement } | null = null;
afterEach(async () => {
  if (mounted) {
    const { root, container } = mounted;
    await act(async () => root.unmount());
    container.remove();
    mounted = null;
  }
  globalThis.fetch = originalFetch;
  if (documentCookie) Object.defineProperty(document, 'cookie', documentCookie);
  else Reflect.deleteProperty(document, 'cookie');
  for (const node of Array.from(document.body.children)) node.remove();
});

const settle = (ms = 0) => act(async () => new Promise((resolve) => setTimeout(resolve, ms)));

async function mount(element: ReturnType<typeof createElement>) {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  mounted = { root, container };
  await act(async () => root.render(element));
  await settle();
  await settle();
  return container;
}

async function openPage() {
  Object.defineProperty(document, 'cookie', { configurable: true, value: 'csrf=token' });
  return mount(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(
        MemoryRouter,
        { initialEntries: ['/swarms/swarm-1'] },
        createElement(Routes, null, [
          createElement(Route, {
            key: 'swarm',
            path: '/swarms/:id',
            element: createElement(SwarmPage),
          }),
        ])
      )
    )
  );
}

const tick = async () => {
  await act(async () => {
    await tickSwarmPolling();
  });
  await settle();
  await settle();
};

const click = (element: Element | null | undefined) =>
  act(async () => (element as HTMLElement).click());

async function submitDialog() {
  const form = document.querySelector('[role="dialog"] form');
  if (!form) throw new Error('no dialog form');
  await act(async () => {
    form.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  });
  await settle();
  await settle();
}

describe('one tick on the page', () => {
  test('a running swarm reads its execution and inbox once per tick, nothing else', async () => {
    const page = stand(run());
    await openPage();
    page.mark();
    await tick();
    expect(page.reads()).toEqual({ run: 1, graph: 0, requests: 1, roles: 0 });
    page.mark();
    await tick();
    expect(page.reads()).toEqual({ run: 1, graph: 0, requests: 1, roles: 0 });
  });

  test('the graph follows the execution: a new current node or status reads it once', async () => {
    const page = stand(run());
    await openPage();
    page.state.run = run({ currentNode: 'verify' });
    page.mark();
    await tick();
    expect(page.reads()).toEqual({ run: 1, graph: 1, requests: 1, roles: 0 });
    // The execution ended: the graph once more, and the roles once by the key change.
    page.state.run = run({ currentNode: 'verify', status: 'succeeded' });
    page.mark();
    await tick();
    expect(page.reads()).toEqual({ run: 1, graph: 1, requests: 1, roles: 1 });
    // A finished execution is not polled; the roles are, since they can change now.
    page.mark();
    await tick();
    expect(page.reads()).toEqual({ run: 0, graph: 0, requests: 1, roles: 1 });
  });

  test('an answer still rereads the execution, the graph and the inbox at once', async () => {
    const page = stand(run({ status: 'waiting_human' }), [nodeAsk()]);
    const container = await openPage();
    const form = container.querySelector('[role="radiogroup"]')?.closest('form');
    if (!form) throw new Error('no answer form');
    const approve = [...form.querySelectorAll('input[type="radio"]')].find((input) =>
      input.closest('label')?.textContent?.includes('Approve')
    );
    page.mark();
    await click(approve);
    await click(form.querySelector('button[type="submit"]'));
    await settle();
    await settle();
    expect(page.calls.filter((c) => c.method === 'POST').length).toBe(1);
    expect(page.reads()).toEqual({ run: 1, graph: 1, requests: 1, roles: 0 });
  });

  test('a stop still rereads the execution, the graph and the inbox at once', async () => {
    const page = stand(run());
    const container = await openPage();
    const stop = [...container.querySelectorAll('[data-swarm-action] button')].find(
      (button) => button.textContent === en.swarm_page.control.stop
    );
    await click(stop);
    page.mark();
    await submitDialog();
    await settle(400);
    expect(page.calls.filter((c) => c.method === 'POST').length).toBe(1);
    // The execution ended: the roles once by the key change; the graph by the
    // explicit refresh and possibly once more for the changed status.
    const reads = page.reads();
    expect(reads.run).toBe(1);
    expect(reads.requests).toBe(1);
    expect(reads.roles).toBe(1);
    expect(reads.graph).toBeGreaterThanOrEqual(1);
  });
});

describe('a hidden tab', () => {
  function Ticker({ onTick }: { onTick: () => void }) {
    const [ticks, setTicks] = useState(0);
    useVisibleInterval(() => {
      onTick();
      setTicks((n) => n + 1);
    }, 5);
    return createElement('output', null, String(ticks));
  }

  const visibility = (state: 'visible' | 'hidden') => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: state });
    Object.defineProperty(document, 'hidden', { configurable: true, value: state === 'hidden' });
    document.dispatchEvent(new window.Event('visibilitychange'));
  };

  test('stops the timer and resumes with a tick when the tab is shown again', async () => {
    let ticks = 0;
    await mount(createElement(Ticker, { onTick: () => void ticks++ }));
    await settle(40);
    expect(ticks).toBeGreaterThan(1);
    await act(async () => visibility('hidden'));
    const atHide = ticks;
    await settle(40);
    expect(ticks).toBe(atHide);
    await act(async () => visibility('visible'));
    await settle(0);
    expect(ticks).toBeGreaterThan(atHide);
    Reflect.deleteProperty(document, 'visibilityState');
    Reflect.deleteProperty(document, 'hidden');
  });
});
