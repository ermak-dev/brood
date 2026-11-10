// FEATURE: The swarm page is the primary panel object (#215, epic #204).
// SPEC: The rendered page is titled by the swarm; known names replace IDs;
// the full swarm ID is a separate copy element; infrastructure and
// execution are separate; stopped and waiting read differently; the
// selected node shows its instruction, outcome and actor; /swarms/:id
// opens exactly the addressed swarm; the swarm list leads to the page.
import { afterAll, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { act, createElement, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun, RunGraph } from '@/services/brood/pipelines';
import { SwarmView } from '@/features/swarms/SwarmPage';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

// DOM-глобалы на время файла: RunGraph разбирает серверный SVG.
const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');

const swarm: AccessSwarm = {
  id: 'swarm-7f3a9c2e5b1d4e6f',
  project_id: 'project-4b8c1d2e',
  owner_id: 'user-9a8b7c6d',
  owner_username: 'alice',
  role: 'owner',
  name: 'release-crew',
  atespace_name: 'brood-7f3a9c2e',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-02T09:00:00Z',
};

const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 80">
<g class="node node-actor state-done" data-node-id="architecture"><rect x="0" y="0" width="50" height="20"/><text x="25" y="14">architecture</text></g>
<g class="node node-human state-active" data-node-id="decision"><rect x="0" y="40" width="50" height="20"/><text x="25" y="54">decision</text></g>
</svg>`;

const graph: RunGraph = {
  svg,
  nodes: [
    { id: 'architecture', kind: 'actor', prompt: 'Propose the architecture' },
    { id: 'decision', kind: 'human', question: 'Approve the proposal?' },
  ],
};

const run = (over: Partial<PipelineRun> = {}): PipelineRun => ({
  id: swarm.id,
  name: 'swarm-architecture',
  status: 'waiting_human',
  swarmId: swarm.id,
  swarmName: swarm.name,
  projectId: swarm.project_id,
  projectName: 'brood',
  owner: 'alice',
  startedAt: null,
  currentNode: 'decision',
  nodes: [
    {
      id: 'architecture',
      kind: 'actor',
      label: 'architecture',
      status: 'done',
      outcome: 'success',
      actorName: 'architect',
      sequence: 1,
    },
    { id: 'decision', kind: 'human', label: 'decision', status: 'active', sequence: 2 },
  ],
  ...over,
});

const retry = async () => {};
type ViewProps = Parameters<typeof SwarmView>[0];
const props = (over: Partial<ViewProps> = {}): ViewProps => ({
  swarm,
  projectName: 'brood',
  run: run(),
  runLoading: false,
  runError: '',
  retryRun: retry,
  graph,
  graphLoading: false,
  graphError: '',
  retryGraph: retry,
  selected: 'decision',
  onSelect: () => {},
  ...over,
});

const wrap = (node: ReactNode) =>
  createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null, node));
const wrapAt = (entry: string, node: ReactNode) =>
  createElement(
    I18nextProvider,
    { i18n },
    createElement(MemoryRouter, { initialEntries: [entry] }, node)
  );
const render = (over: Partial<ViewProps> = {}) =>
  renderToStaticMarkup(wrap(createElement(SwarmView, props(over))));

/** Текст разметки без тегов и атрибутов: то, что видит человек. */
const visibleText = (html: string) => html.replace(/<[^>]*>/g, ' ');

describe('identity', () => {
  test('the page is titled by the swarm, never by the graph definition', () => {
    const html = render();
    const title = html.match(/<h1[^>]*>(.*?)<\/h1>/)?.[1];
    expect(title).toBe('release-crew');
    expect(title).not.toContain('swarm-architecture');
  });

  test('known names of the project, owner and actor are shown instead of IDs', () => {
    const html = render({ selected: 'architecture' });
    const text = visibleText(html);
    expect(text).toContain('brood');
    expect(text).toContain('alice');
    expect(text).toContain('architect');
    expect(text).not.toContain(swarm.project_id);
    expect(text).not.toContain(swarm.owner_id);
    // The project is a link to its page, named, with the ID only in the address.
    expect(html).toContain(`href="/projects/${swarm.project_id}"`);
  });

  test('the full swarm ID is a separate copy element, not the title', () => {
    const html = render();
    const text = visibleText(html);
    expect(text.split(swarm.id).length - 1).toBe(1);
    expect(html).toMatch(new RegExp(`<code[^>]*>${swarm.id}</code>`));
    expect(html).toContain(`aria-label="${en.swarm_page.copy_id}"`);
  });
});

describe('state', () => {
  const card = (html: string, label: string) => {
    const start = html.indexOf(`>${label}<`);
    return html.slice(start, html.indexOf('</section>', start));
  };

  test('infrastructure and execution are separate cards with their own badges', () => {
    const html = render();
    const infrastructure = card(html, en.swarm_page.infrastructure);
    const execution = card(html, en.swarm_page.execution);
    expect(infrastructure).toContain(en.access.states.active);
    expect(infrastructure).not.toContain(en.swarm_page.execution_states.waiting);
    expect(execution).toContain(en.swarm_page.execution_states.waiting);
    expect(execution).not.toContain(en.access.states.active);
  });

  test('a stopped swarm and a swarm waiting for an answer read differently', () => {
    const waiting = card(render(), en.swarm_page.execution);
    const stopped = card(render({ run: run({ status: 'stopped' }) }), en.swarm_page.execution);
    expect(waiting).toContain(en.swarm_page.execution_hints.waiting);
    expect(stopped).toContain(en.swarm_page.execution_states.stopped);
    expect(stopped).toContain(en.swarm_page.execution_hints.stopped);
    expect(stopped).not.toContain(en.swarm_page.execution_states.waiting);
    // Тон значка различается тоже: executionTone в swarmPage.test.ts.
  });

  test('a swarm without a graph says so instead of drawing an empty graph', () => {
    const html = render({ run: null, graph: null, selected: undefined });
    expect(html).toContain(en.swarm_page.execution_hints.no_graph);
    expect(html).not.toContain('role="group"');
    expect(html).not.toContain(en.swarm_page.steps);
  });
});

describe('selected node', () => {
  test('the graph marks the selected node and the details follow it', () => {
    const html = render({ selected: 'architecture' });
    expect(html).toMatch(
      /data-node-id="architecture"[^>]*aria-pressed="true"|aria-pressed="true"[^>]*data-node-id="architecture"/
    );
    expect(html).toContain('Propose the architecture');
    expect(html).not.toContain('Approve the proposal?');
  });

  test('a human node shows its question; the gate form appears only on that node', () => {
    const gate = createElement('form', { 'data-gate': 'yes' });
    expect(render({ selected: 'decision', gateNode: 'decision', gate })).toContain('data-gate');
    expect(render({ selected: 'decision', gateNode: 'decision', gate })).toContain(
      'Approve the proposal?'
    );
    expect(render({ selected: 'architecture', gateNode: 'decision', gate })).not.toContain(
      'data-gate'
    );
  });

  test('step moments appear only when the server recorded them', () => {
    expect(render()).not.toContain(`>${en.swarm_page.started}<`);
    const timed = run({
      nodes: [
        {
          id: 'architecture',
          kind: 'actor',
          label: 'architecture',
          status: 'done',
          sequence: 1,
          startedAt: '2026-11-03T09:00:00Z',
          finishedAt: '2026-11-03T09:10:00Z',
        },
      ],
    });
    expect(render({ run: timed, selected: 'architecture' })).toContain(
      `>${en.swarm_page.started}<`
    );
  });

  // BUG: An unconfirmed actor visit had no visible uncertainty in the timeline.
  // HYPOTHESIS: The timeline displayed outcomes but not the unconfirmed state.
  // SOLUTION: Show the localized unknown-outcome state, in words.
  // RESULT: FIXED in the run page; the step history keeps it after #217.
  // STATUS: FIXED
  test('an unconfirmed step says its outcome is unknown, not that it failed', () => {
    const unconfirmed = run({
      nodes: [
        {
          id: 'architecture',
          kind: 'actor',
          label: 'architecture',
          status: 'unconfirmed',
          sequence: 1,
        },
      ],
    });
    const text = visibleText(render({ run: unconfirmed, selected: undefined }));
    expect(text).toContain(en.swarm_page.node_states.unconfirmed);
    expect(text).not.toContain(en.swarm_page.node_states.failed);
  });
});

describe('route and list', () => {
  // MainRoutes целиком в bun test не импортируется (страницы квот требуют
  // настоящих CSS-модулей), поэтому путь берётся из таблицы маршрутов, а
  // открывает его настоящий SwarmPage.
  test('a direct address opens exactly the addressed swarm', async () => {
    const routes = source('router/MainRoutes.tsx');
    const path = routes.match(/path: '([^']+)', element: <SwarmPage \/>/)?.[1];
    expect(path).toBe('/swarms/:id');
    const { SwarmPage } = await import('@/features/swarms/SwarmPage');
    const requested: string[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (url: string) => {
      requested.push(url);
      const body =
        url === `/api/swarms/${encodeURIComponent('s/2')}`
          ? { ...swarm, id: 's/2', name: 'second-crew' }
          : url === '/api/projects'
            ? [{ id: swarm.project_id, name: 'brood' }]
            : { detail: 'not found' };
      return new Response(JSON.stringify(body), { status: 'detail' in body ? 404 : 200 });
    }) as typeof fetch;
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    try {
      await act(async () =>
        root.render(
          wrapAt(
            `/swarms/${encodeURIComponent('s/2')}`,
            createElement(Routes, null, [
              createElement(Route, { key: 'list', path: '/swarms', element: 'list' }),
              createElement(Route, { key: 'swarm', path, element: createElement(SwarmPage) }),
            ])
          )
        )
      );
      await act(async () => new Promise((resolve) => setTimeout(resolve, 0)));
      expect(requested).toContain('/api/swarms/s%2F2');
      expect(requested).not.toContain('/api/swarms/s');
      expect(container.querySelector('h1')?.textContent).toBe('second-crew');
      // Рой без графа: исполнение «не начат», граф не рисуется.
      expect(container.textContent).toContain(en.swarm_page.execution_hints.no_graph);
    } finally {
      await act(async () => root.unmount());
      container.remove();
      globalThis.fetch = originalFetch;
    }
  });

  test('the route table names the swarm page and members may open it', async () => {
    const { visibleAccessPath } = await import('@/features/access/permissions');
    expect(source('router/MainRoutes.tsx')).toContain(
      "path: '/swarms/:id', element: <SwarmPage />"
    );
    expect(visibleAccessPath('/swarms/:id', false)).toBe(true);
  });

  test('a row of the swarm list opens the swarm page, not a side panel', () => {
    const list = source('features/access/SwarmsPage.tsx');
    expect(list).toContain('onRowClick={(swarm) => navigate(swarmPath(swarm.id))}');
    expect(list).not.toContain('<EntityPanel');
    // Access and deletion stay available on the page, after the work.
    const page = source('features/swarms/SwarmPage.tsx');
    const management = page.indexOf('{management}');
    expect(management).toBeGreaterThan(page.indexOf('<StepHistory'));
  });
});
