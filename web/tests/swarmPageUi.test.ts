// FEATURE: The swarm page is the primary panel object (#215, epic #204).
// SPEC: The rendered page is titled by the swarm; known names replace IDs;
// the full swarm ID is a separate copy element; infrastructure and
// execution are separate; stopped and waiting read differently; the
// selected node shows its instruction, outcome and actor; /swarms/:id
// opens exactly the addressed swarm; the swarm list leads to the page.
// The goal of the current pass stands under the states as text, line breaks
// kept; the step history splits into passes under their goals (#242).
import { afterAll, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { act, createElement, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import type { NodeStatus, PipelineRun, RunGraph, RunNode } from '@/services/brood/pipelines';
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

describe('actor step states (#212, #238)', () => {
  const sha = '0123456789abcdef0123456789abcdef01234567';
  const actorStep = (over: Partial<RunNode> = {}): RunNode => ({
    id: 'architecture',
    kind: 'actor',
    label: 'architecture',
    status: 'refused',
    actorName: 'architect',
    sequence: 1,
    ...over,
  });
  const stuck = (over: Partial<RunNode> = {}, nodes: RunNode[] = []) =>
    run({ status: 'running', currentNode: 'architecture', nodes: [...nodes, actorStep(over)] });
  const count = (text: string, part: string) => text.split(part).length - 1;
  /** Видимый текст с раскрытыми сущностями: апостроф перевода — не &#x27;. */
  const shown = (html: string) =>
    visibleText(html)
      .replace(/&#x27;/g, "'")
      .replace(/&quot;/g, '"')
      .replace(/&lt;/g, '<')
      .replace(/&gt;/g, '>')
      .replace(/&amp;/g, '&');
  /** Панель узла и история шагов: то, что ниже графа. */
  const work = (html: string) => shown(html.slice(html.indexOf(`>${en.swarm_page.node}<`)));
  const states = en.swarm_page.node_states as Record<string, string>;
  const guidance = en.swarm_page.step_guidance;

  test('every step state reads in words in the node panel and in the history', () => {
    for (const status of [
      'pending',
      'attesting',
      'delivering',
      'inflight',
      'active',
      'completed',
      'done',
      'refused',
      'unconfirmed',
      'stopped',
    ] as NodeStatus[]) {
      const text = work(render({ run: stuck({ status }), selected: 'architecture' }));
      expect({ status, word: states[status], shown: count(text, ` ${states[status]} `) }).toEqual({
        status,
        word: expect.any(String),
        shown: 2,
      });
    }
  });

  test('a refused step names its reason and says what to do once', () => {
    const detail = 'actor architect is not connected to the relay; no instruction was sent';
    const html = render({ run: stuck({ detail }), selected: 'architecture' });
    const text = shown(html);
    // The reason is on the node and on its step in the history.
    expect(count(text, detail)).toBe(2);
    expect(text).toContain(` ${en.swarm_page.reason} `);
    expect(count(text, guidance.refused)).toBe(1);
    expect(text).not.toContain(guidance.unconfirmed);
    expect(text).not.toContain('bro dispatch');
    expect(html).toContain('role="note"');
  });

  test('an unconfirmed step leaves the decision to the operator, with the lease release', () => {
    const detail =
      'workspace attestation outcome is unknown; workspace operation op-7 stays reserved until an operator releases it; no instruction was sent';
    const text = shown(
      render({ run: stuck({ status: 'unconfirmed', detail }), selected: 'architecture' })
    );
    // The operation ID is a monospace element; the detail itself is unchanged.
    expect(count(text.replace(/\s+/g, ' '), detail)).toBe(2);
    expect(count(text, guidance.unconfirmed)).toBe(1);
    expect(text).toContain(guidance.release);
    expect(text).toContain('bro dispatch architect --request-id');
    expect(text).toContain(`--action release --swarm ${swarm.id}`);
    expect(text).not.toContain(guidance.refused);
    expect(text).not.toContain(` ${states.failed} `);
  });

  test('a refusal an earlier pass already answered gives no instruction', () => {
    const detail = 'the step changed before workspace attestation; no instruction was sent';
    const later = run({
      status: 'running',
      currentNode: 'decision',
      nodes: [
        actorStep({ detail }),
        { id: 'decision', kind: 'human', label: 'decision', status: 'active', sequence: 2 },
      ],
    });
    const text = shown(render({ run: later, selected: 'architecture' }));
    expect(count(text, detail)).toBe(2);
    expect(text).not.toContain(guidance.refused);
  });

  test('the confirmed commit is short on screen, whole in its title and its copy button', () => {
    const html = render({
      run: stuck({ status: 'done', outcome: 'success', commit: sha }),
      selected: 'architecture',
    });
    const text = visibleText(html);
    expect(count(text, sha.slice(0, 8))).toBe(2);
    expect(text).not.toContain(sha);
    expect(count(html, `title="${sha}"`)).toBe(2);
    expect(count(html, `aria-label="${en.swarm_page.copy_commit}"`)).toBe(2);
    expect(text).toContain(en.swarm_page.commit);
  });

  test('the step report is text with its line breaks, never markup', () => {
    const report = 'Done: <b>branch</b> feat/x\nMR !12 & docs';
    const html = render({
      run: stuck({ status: 'done', outcome: 'success', report }),
      selected: 'architecture',
    });
    expect(html).not.toContain('<b>branch</b>');
    expect(html).toContain('Done: &lt;b&gt;branch&lt;/b&gt; feat/x\nMR !12 &amp; docs');
    // Full in the node panel, folded in the history.
    expect(count(html, '&lt;b&gt;branch')).toBe(2);
    expect(html).toContain('<details');
    expect(html).toContain(`>${en.swarm_page.report}</summary>`);
  });

  test('a step without reason, commit or report shows none of them', () => {
    const text = shown(render({ run: stuck({ status: 'inflight' }), selected: 'architecture' }));
    expect(text).not.toContain(` ${en.swarm_page.reason} `);
    expect(text).not.toContain(` ${en.swarm_page.commit} `);
    expect(text).not.toContain(` ${en.swarm_page.report} `);
  });
});

describe('pass goal', () => {
  const goal = 'Fix issue #12\n\t- keep the API\n<b>as is</b>';
  const passes = run({
    goal: 'Fix issue #13',
    nodes: [
      {
        id: 'architecture',
        kind: 'actor',
        label: 'architecture',
        status: 'done',
        sequence: 1,
        pass: 1,
        goal,
      },
      { id: 'decision', kind: 'human', label: 'decision', status: 'done', sequence: 2, pass: 1 },
      {
        id: 'architecture',
        kind: 'actor',
        label: 'architecture',
        status: 'active',
        sequence: 3,
        pass: 3,
        goal: 'Fix issue #13',
      },
    ],
  });
  const goalCard = (html: string) => {
    const start = html.indexOf('data-pass-goal');
    return start < 0 ? '' : html.slice(start, html.indexOf('</section>', start));
  };

  test('the goal of the current pass is markdown without HTML, line breaks kept', () => {
    const html = render({ run: run({ goal, nodes: passes.nodes.slice(0, 2) }) });
    const card = goalCard(html);
    expect(card).toContain(en.swarm_page.pass_goal.label);
    expect(card).toContain('data-goal-text=""');
    // Markdown (#252): the author's tag is text, the line breaks stay.
    expect(card).toContain('<p>Fix issue #12\n- keep the API\n&lt;b&gt;as is&lt;/b&gt;</p>');
    expect(card).not.toContain('<b>');
    // The pass is named by its first step.
    expect(visibleText(card)).toContain(
      en.swarm_page.pass_goal.pass.replace('{{number}}', '1').replace('{{step}}', '1')
    );
  });

  test('the card follows the current pass, not an earlier one', () => {
    const card = goalCard(render({ run: passes }));
    expect(card).toContain('Fix issue #13');
    expect(card).not.toContain('Fix issue #12');
    expect(visibleText(card)).toContain(
      en.swarm_page.pass_goal.pass.replace('{{number}}', '2').replace('{{step}}', '3')
    );
  });

  test('a swarm that has not started a pass has no goal card', () => {
    expect(goalCard(render())).toBe('');
    expect(goalCard(render({ run: null, graph: null, selected: undefined }))).toBe('');
  });

  test('the goal text keeps its line breaks on screen', () => {
    const styles = source('features/swarms/SwarmPage.module.scss');
    const rule = styles.slice(styles.indexOf('.goalText'));
    expect(rule.slice(0, rule.indexOf('}'))).toContain('white-space: pre-wrap');
  });

  test('the step history splits into passes, each under its own goal', () => {
    const html = render({ run: passes, selected: undefined });
    const history = html.slice(html.indexOf(`>${en.swarm_page.steps}<`));
    const first = history.indexOf('data-pass="1"');
    const second = history.indexOf('data-pass="3"');
    expect(first).toBeGreaterThan(0);
    expect(second).toBeGreaterThan(first);
    const earlier = history.slice(first, second);
    const later = history.slice(second);
    expect(earlier).toContain('&lt;b&gt;as is&lt;/b&gt;');
    expect(earlier).toContain('#1<');
    expect(earlier).toContain('#2<');
    expect(earlier).not.toContain('#3<');
    expect(later).toContain('Fix issue #13');
    expect(later).toContain('#3<');
    expect(visibleText(later)).toContain(
      en.swarm_page.pass_goal.pass.replace('{{number}}', '2').replace('{{step}}', '3')
    );
  });

  test('steps without passes keep the plain history', () => {
    const html = render({ selected: undefined });
    expect(html).not.toContain('data-pass=');
    expect(html).toContain('#1<');
    expect(html).toContain('#2<');
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
