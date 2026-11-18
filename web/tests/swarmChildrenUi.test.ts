// FEATURE: Панель показывает историю детей и подтверждает только безопасный abort.
// SPEC: Чужая роль и неизвестный эффект не получают действие; отчёты — текст,
// подтверждение обязательно, поздний ответ старого scope не меняет новый рой.
import { afterAll, afterEach, expect, test } from 'bun:test';
import { act, createElement, type ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter } from 'react-router-dom';
import { readFileSync } from 'node:fs';
import type { AccessSwarm } from '@/services/brood/access';
import { ChildGroupDetailsView, ChildGroupsPane } from '@/features/swarms/SwarmChildren';
import { SwarmView } from '@/features/swarms/SwarmPage';
import type { PipelineRun } from '@/services/brood/pipelines';
import { childGroup, childSummary, childTask } from './fixtures/swarmChildren';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
afterAll(restoreDOM);
const i18n = createInstance();
await i18n.init({
  lng: 'en',
  resources: { en: { translation: en } },
  interpolation: { escapeValue: false },
});
const wrap = (node: ReactNode) =>
  createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null, node));
const swarm = (id = 'swarm-a'): AccessSwarm => ({
  id,
  project_id: 'project-1',
  owner_id: 'user-1',
  owner_username: 'alice',
  role: 'owner',
  name: id,
  atespace_name: id,
  state: 'active',
  created_at: 'now',
  updated_at: 'now',
});

const markup = (group = childGroup(), owner = true, stale = false) =>
  renderToStaticMarkup(
    wrap(
      createElement(ChildGroupDetailsView, {
        group,
        owner,
        stale,
        busy: false,
        onAbort: () => {},
      })
    )
  );

test('swarm page retains the child history slot with and without a pinned graph', () => {
  const running: PipelineRun = {
    id: 'swarm-a',
    name: 'develop',
    status: 'running',
    swarmId: 'swarm-a',
    projectId: 'project-1',
    projectName: 'project',
    startedAt: null,
    currentNode: '',
    nodes: [],
    definition: { name: 'develop', version: 1, revision: 'rev' },
  };
  for (const run of [null, running]) {
    const html = renderToStaticMarkup(
      wrap(
        createElement(SwarmView, {
          swarm: swarm(),
          projectName: 'project',
          run,
          runLoading: false,
          runError: '',
          retryRun: async () => {},
          graph: null,
          graphLoading: false,
          graphError: '',
          retryGraph: async () => {},
          onSelect: () => {},
          childTasks: createElement('div', { 'data-child-history-slot': '' }, 'Recorded children'),
        })
      )
    );
    const document = new DOMParser().parseFromString(html, 'text/html');
    expect(document.querySelectorAll('[data-child-history-slot]')).toHaveLength(1);
  }
});

test('owner sees abort only for an incomplete known-unsent group', () => {
  expect(markup()).toContain('data-child-abort');
  expect(markup(childGroup(), false)).not.toContain('data-child-abort');
  expect(markup(childGroup(), true, true)).not.toContain('data-child-abort');
  for (const group of [
    childGroup({
      started: 1,
      children: [childTask({ state: 'running', delivery: 'unconfirmed' })],
    }),
    childGroup({ started: 1, children: [childTask({ state: 'completed', outcome: 'success' })] }),
    childGroup({ registered: 2, children: [childTask(), childTask({ id: 'child-2' })] }),
    childGroup({ state: 'aborted', open: 0 }),
  ])
    expect(markup(group)).not.toContain('data-child-abort');
});

test('delivery and join readiness do not claim parent completion; reports are escaped', () => {
  const report = '<script>do not execute</script>\nIndependent report.';
  const html = markup(
    childGroup({
      mode: 'any',
      readiness: 'ready_success',
      started: 1,
      open: 1,
      children: [
        childTask({ state: 'completed', delivery: 'delivered', outcome: 'success', report }),
      ],
    })
  );
  const document = new DOMParser().parseFromString(html, 'text/html');
  expect(document.querySelector('[data-child-group-state]')?.textContent).toContain('Open');
  expect(document.querySelector('[data-child-readiness]')?.textContent).toContain(
    'Success threshold reached'
  );
  expect(document.body.textContent).toContain('Readiness does not confirm parent completion.');
  expect(document.body.textContent).toContain('executor-original');
  expect(document.body.textContent).toContain('incarnation 4');
  expect(document.body.textContent).toContain(report);
  expect(document.querySelectorAll('script')).toHaveLength(0);
  expect(document.querySelector('[data-child-abort]')).toBeNull();
});

test('all six locales contain the child task labels and state names', () => {
  const keys = (value: unknown, prefix = ''): string[] => {
    if (typeof value === 'string') return [prefix];
    if (!value || typeof value !== 'object') return [];
    return Object.entries(value)
      .flatMap(([key, item]) => keys(item, `${prefix}.${key}`))
      .sort();
  };
  const namespaces = ['en', 'ru', 'zh-CN', 'zh-TW', 'vi', 'ko'].map(
    (locale) =>
      JSON.parse(
        readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
      ).swarm_children
  );
  for (const namespace of namespaces) {
    expect(namespace).toBeDefined();
    expect(keys(namespace)).toEqual(keys(namespaces[0]));
    expect(namespace.title).toBeTruthy();
    expect(namespace.abort_confirm).toBeTruthy();
    expect(namespace.child_states.running).toBeTruthy();
    expect(namespace.readiness_states.unconfirmed).toBeTruthy();
  }
});

const originalFetch = globalThis.fetch;
let mounted: { root: Root; container: HTMLElement } | null = null;
afterEach(async () => {
  if (mounted) {
    await act(async () => mounted?.root.unmount());
    mounted.container.remove();
    mounted = null;
  }
  globalThis.fetch = originalFetch;
  for (const node of Array.from(document.body.children)) node.remove();
});
const flush = () =>
  act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
async function mount(owner = true) {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  document.cookie = 'csrf=fixture-csrf';
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  mounted = { root, container };
  await act(async () =>
    root.render(wrap(createElement(ChildGroupsPane, { swarm: swarm(), owner })))
  );
  await flush();
  return container;
}
async function click(element: Element | null) {
  if (!element) throw new Error('required child history control is missing');
  await act(async () => (element as HTMLElement).click());
  await flush();
}

test('abort is never posted before confirmation and renders retained cancelled intent', async () => {
  let aborted = false;
  const writes: unknown[] = [];
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      writes.push(JSON.parse(String(init.body)));
      aborted = true;
      return Response.json({ state: 'aborted', revision: 2, open: 0 });
    }
    const group = aborted
      ? childGroup({
          state: 'aborted',
          revision: 2,
          readiness: 'aborted',
          open: 0,
          abortedBy: 'alice',
          abortedAt: '2026-11-15T11:00:00Z',
          children: [childTask({ state: 'cancelled', revision: 2 })],
        })
      : childGroup();
    return Response.json(
      url.includes('?')
        ? {
            items: [
              aborted
                ? { ...childSummary(), state: 'aborted', revision: 2, open: 0 }
                : childSummary(),
            ],
            nextBefore: 0,
          }
        : group
    );
  }) as typeof fetch;
  const container = await mount();
  await click(container.querySelector('[data-child-select="3"]'));
  expect(writes).toEqual([]);
  await click(container.querySelector('[data-child-abort]'));
  expect(writes).toEqual([]);
  const confirmation = document.querySelector('[role="dialog"]');
  expect(confirmation?.textContent).toContain('does not undo running or unknown external effects');
  const buttons = Array.from(confirmation?.querySelectorAll('button') ?? []);
  await click(buttons.find((button) => button.textContent?.trim() === 'Abort this group') ?? null);
  expect(writes).toEqual([{ revision: 1, confirm: true }]);
  expect(container.textContent).toContain(
    'Unstarted group aborted. History is retained; parent completion is not confirmed.'
  );
  expect(container.querySelector('[data-child-group-state]')?.textContent).toContain('Aborted');
  expect(container.querySelector('[data-child-task-state]')?.textContent).toContain('Cancelled');
  expect(container.querySelector('[data-child-abort]')).toBeNull();
});

test('late abort failure from an unmounted swarm never appears on a new swarm', async () => {
  let finish: (response: Response) => void = () => {};
  let writes = 0;
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      writes++;
      return new Promise<Response>((resolve) => {
        finish = resolve;
      });
    }
    const group = childGroup({
      children: [
        childTask({ task: url.includes('swarm-b') ? 'New swarm task' : 'Old swarm task' }),
      ],
    });
    return Response.json(url.includes('?') ? { items: [childSummary()], nextBefore: 0 } : group);
  }) as typeof fetch;
  const container = await mount();
  await click(container.querySelector('[data-child-select="3"]'));
  await click(container.querySelector('[data-child-abort]'));
  await click(
    Array.from(document.querySelector('[role="dialog"]')?.querySelectorAll('button') ?? []).find(
      (button) => button.textContent?.trim() === 'Abort this group'
    ) ?? null
  );
  expect(writes).toBe(1);
  await act(async () =>
    mounted?.root.render(
      wrap(createElement(ChildGroupsPane, { swarm: swarm('swarm-b'), owner: false }))
    )
  );
  await flush();
  await click(container.querySelector('[data-child-select="3"]'));
  await act(async () => finish(Response.json({ detail: 'old-scope-failure' }, { status: 500 })));
  await flush();
  expect(container.textContent).toContain('New swarm task');
  expect(container.textContent).not.toContain('Old swarm task');
  expect(container.textContent).not.toContain('old-scope-failure');
  expect(container.querySelector('[data-child-abort]')).toBeNull();
});
