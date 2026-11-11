// FEATURE: Start and stop the swarm from its page (#218, API #211, page #215).
// SPEC: The header carries the one execution action the user may take. The
// first start picks a definition from the catalog of the swarm's project,
// previews its graph with the same graph component before the start and
// sends the commit only when one is typed; a restart runs on the pinned
// snapshot without a picker; stop asks first and says actors and workspaces
// stay. Every start asks for the goal of its pass (#242): a restart starts
// with an empty field and shows the goal of the previous pass; a goal the
// server would refuse does not leave the dialog, its reason stays at the
// field, and the byte counter follows the typed text. After the action the page reads the swarm again; a refusal shows
// the server's reason in the dialog. Next to the graph the page shows the
// snapshot identity the swarm recorded: a newer catalog version changes
// neither the identity nor the graph the page draws.
import { afterAll, afterEach, describe, expect, test } from 'bun:test';
import { act, createElement, type ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun, RunGraph } from '@/services/brood/pipelines';
import { SwarmPage, SwarmView } from '@/features/swarms/SwarmPage';
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

const control = en.swarm_page.control;
const snapshot = en.swarm_page.snapshot;
const pinned = '0123456789abcdef0123456789abcdef';
const newer = 'fedcba9876543210fedcba9876543210';
const commit = '89abcdef0123456789abcdef0123456789abcdef';

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

const snapshotSvg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 80">
<g class="node node-human state-active" data-node-id="ask"><rect x="0" y="0" width="50" height="20"/><text x="25" y="14">ask</text></g>
</svg>`;
const catalogSvg = (node: string) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 80">
<g class="node node-actor" data-node-id="${node}"><rect x="0" y="0" width="50" height="20"/><text x="25" y="14">${node}</text></g>
</svg>`;

const run = (over: Partial<PipelineRun> = {}): PipelineRun => ({
  id: 'swarm-1',
  name: 'release',
  definition: { name: 'release', version: 1, revision: pinned },
  status: 'stopped',
  swarmId: 'swarm-1',
  swarmName: 'release-crew',
  projectId: 'project-1',
  projectName: 'brood',
  owner: 'alice',
  startedAt: null,
  currentNode: 'ask',
  nodes: [{ id: 'ask', kind: 'human', label: 'ask', status: 'skipped', sequence: 1 }],
  ...over,
});

// Каталог проекта ушёл вперёд: release уже версии 2, снимок роя — версии 1.
const catalog = {
  workflows: [
    { name: 'release', version: 2, revision: newer, publishedAt: '2026-11-03T09:00:00Z' },
    { name: 'review', version: 1, revision: pinned, publishedAt: null },
  ],
};

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

const originalFetch = globalThis.fetch;
let mounted: { root: Root; container: HTMLElement } | null = null;
afterEach(async () => {
  if (mounted) {
    const { root, container } = mounted;
    await act(async () => root.unmount());
    container.remove();
    mounted = null;
  }
  globalThis.fetch = originalFetch;
  for (const node of Array.from(document.body.children)) node.remove();
});

const settle = (ms = 0) => act(async () => new Promise((resolve) => setTimeout(resolve, ms)));

async function openPage(id = 'swarm-1') {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  mounted = { root, container };
  await act(async () =>
    root.render(
      createElement(
        I18nextProvider,
        { i18n },
        createElement(
          MemoryRouter,
          { initialEntries: [`/swarms/${id}`] },
          createElement(Routes, null, [
            createElement(Route, {
              key: 'swarm',
              path: '/swarms/:id',
              element: createElement(SwarmPage),
            }),
          ])
        )
      )
    )
  );
  await settle();
  await settle();
  return container;
}

/** Действие исполнения стоит рядом со строкой состояния (#253), не в шапке. */
const header = (container: HTMLElement) =>
  container.querySelector('[data-swarm-action]') as HTMLElement;
const headerButtons = (container: HTMLElement) =>
  Array.from(header(container)?.querySelectorAll('button') ?? []).map((b) => b.textContent);
const dialog = () => document.querySelector('[role="dialog"]') as HTMLElement | null;

async function click(element: Element | null | undefined) {
  if (!element) throw new Error('nothing to click');
  await act(async () => (element as HTMLElement).click());
  await settle();
  await settle();
}

/** Ввод в поле: значение ставит сеттер прототипа, как браузер. React под
 * happy-dom событий input не видит (react-dom загружен до DOM), поэтому
 * поле, которое читается при отправке, проверяется именно так. */
function type(input: HTMLInputElement, value: string) {
  let proto: object | null = Object.getPrototypeOf(input) as object;
  let setter: ((value: string) => void) | undefined;
  while (proto && !setter) {
    setter = Object.getOwnPropertyDescriptor(proto, 'value')?.set;
    proto = Object.getPrototypeOf(proto) as object | null;
  }
  if (!setter) throw new Error('no value setter');
  setter.call(input, value);
}

/** Ввод в управляемое поле: значение — сеттером прототипа, обработчик React —
 * тем же событием, что дал бы браузер (событие input до onChange не доходит:
 * react-dom загружен раньше DOM). */
async function typeText(field: HTMLTextAreaElement | null, value: string) {
  if (!field) throw new Error('no field to type into');
  await act(async () => {
    type(field as unknown as HTMLInputElement, value);
    const props = Object.entries(field).find(([key]) => key.startsWith('__reactProps$'))?.[1] as
      { onChange?: (event: { target: HTMLTextAreaElement }) => void } | undefined;
    props?.onChange?.({ target: field });
  });
  await settle();
}

const goalField = () =>
  dialog()?.querySelector('textarea[name="goal"]') as HTMLTextAreaElement | null;
const posts = (calls: Call[]) => calls.filter((c) => c.method === 'POST');

async function submit() {
  const form = dialog()?.querySelector('form');
  if (!form) throw new Error('no dialog form');
  await act(async () => {
    form.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  });
  await settle();
  await settle();
}

const pageRoutes = (current: () => PipelineRun | null, over: Partial<AccessSwarm> = {}) => ({
  'GET /api/swarms/swarm-1': { body: swarm(over) },
  'GET /api/projects': { body: [{ id: 'project-1', name: 'brood' }] },
  'GET /api/pipeline-runs/swarm-1': () => {
    const value = current();
    return value ? { body: value } : { status: 404, body: { detail: 'pipeline run not found' } };
  },
  'GET /api/pipeline-runs/swarm-1/graph': {
    body: { svg: snapshotSvg, nodes: [{ id: 'ask', kind: 'human', question: 'Ship?' }] },
  },
  'GET /api/projects/project-1/workflow-catalog': { body: catalog },
});

describe('header', () => {
  const view = (actions: ReactNode, over: Partial<PipelineRun> = {}) =>
    renderToStaticMarkup(
      createElement(
        I18nextProvider,
        { i18n },
        createElement(
          MemoryRouter,
          null,
          createElement(SwarmView, {
            swarm: swarm(),
            projectName: 'brood',
            run: run(over),
            runLoading: false,
            runError: '',
            retryRun: async () => {},
            graph: { svg: snapshotSvg } satisfies RunGraph,
            graphLoading: false,
            graphError: '',
            retryGraph: async () => {},
            onSelect: () => {},
            actions,
          })
        )
      )
    );

  test('the execution action sits beside the execution state, under the header', () => {
    const html = view(createElement('button', { 'data-action': 'yes' }, 'act'));
    const head = html.slice(html.indexOf('<header'), html.indexOf('</header>'));
    expect(head).not.toContain('data-action="yes"');
    const line = html.slice(html.indexOf('data-swarm-now'), html.indexOf(`>${en.swarm_page.graph}<`));
    expect(line).toContain('data-swarm-action');
    expect(line).toContain('data-action="yes"');
  });

  // #253: the identity is one caption line under the graph title, the
  // revision short on screen and whole in its title and its copy button.
  test('the snapshot identity the swarm recorded stands next to its graph', () => {
    const html = view(null);
    const graphSection = html.slice(html.indexOf(`>${en.swarm_page.graph}<`));
    const identity = graphSection.slice(0, graphSection.indexOf('role="group"'));
    expect(identity).toContain('data-snapshot');
    expect(identity).toContain(snapshot.label);
    expect(identity).toContain('release');
    expect(identity).toContain(snapshot.version_short.replace('{{version}}', '1'));
    expect(identity).toContain(`title="${pinned}"`);
    expect(identity).toContain(`>${pinned.slice(0, 8)}<`);
    expect(identity).toContain(`aria-label="${snapshot.copy_revision}"`);
    expect(identity.replace(/<[^>]*>/g, ' ')).not.toContain(pinned);
  });

  test('a run without a recorded identity shows no made-up one', () => {
    expect(view(null, { definition: undefined })).not.toContain('data-snapshot');
  });
});

describe('snapshot, not catalog', () => {
  test('a swarm on version 1 keeps showing version 1 and its own graph after version 2 is published', async () => {
    const calls = api(pageRoutes(() => run()));
    const container = await openPage();
    const text = container.textContent ?? '';
    expect(text).toContain(pinned.slice(0, 8));
    expect(text).not.toContain(newer.slice(0, 8));
    expect(container.querySelector(`[title="${pinned}"]`)).not.toBeNull();
    expect(container.querySelector('[data-node-id="ask"]')).not.toBeNull();
    // The page draws the swarm's own graph and never reads the catalog for it.
    expect(calls.map((c) => c.url)).toContain('/api/pipeline-runs/swarm-1/graph');
    expect(calls.some((c) => c.url.includes('/workflows/'))).toBe(false);
    expect(calls.some((c) => c.url.includes('workflow-catalog'))).toBe(false);
  });
});

describe('who sees which action', () => {
  test('a viewer gets no execution action at all', async () => {
    api(pageRoutes(() => run({ status: 'running' }), { role: 'viewer' }));
    const container = await openPage();
    expect(headerButtons(container)).toEqual([]);
  });

  test('a swarm that is not active gets no execution action', async () => {
    api(pageRoutes(() => null, { state: 'provisioning' }));
    const container = await openPage();
    expect(headerButtons(container)).toEqual([]);
  });

  test('an owner sees start, start again or stop by the execution state', async () => {
    api(pageRoutes(() => null));
    expect(headerButtons(await openPage())).toEqual([control.start]);
  });

  test('a running swarm offers stop only', async () => {
    api(pageRoutes(() => run({ status: 'running' })));
    expect(headerButtons(await openPage())).toEqual([control.stop]);
  });

  test('a stopped swarm offers start again only', async () => {
    api(pageRoutes(() => run({ status: 'stopped' })));
    expect(headerButtons(await openPage())).toEqual([control.restart]);
  });
});

describe('first start', () => {
  test('picks a catalog definition, previews its graph and starts it with the commit', async () => {
    let current: PipelineRun | null = null;
    const calls = api({
      ...pageRoutes(() => current),
      'GET /api/projects/project-1/workflows/review/graph?version=1&revision=0123456789abcdef0123456789abcdef':
        { body: { svg: catalogSvg('check') } },
      'POST /api/swarms/swarm-1/start': () => {
        current = run({
          name: 'review',
          definition: { name: 'review', version: 1, revision: pinned },
          status: 'waiting_human',
        });
        return { body: { swarmId: 'swarm-1', execution: 'waiting' } };
      },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    const opened = dialog();
    expect(opened?.textContent).toContain('release-crew');
    // The catalog of the swarm's own project, with versions.
    expect(calls.map((c) => c.url)).toContain('/api/projects/project-1/workflow-catalog');
    const radios = Array.from(opened?.querySelectorAll('input[type="radio"]') ?? []);
    expect(radios.map((r) => (r as HTMLInputElement).value)).toEqual(['release', 'review']);
    expect(opened?.textContent).toContain('version 2');
    // Nothing is chosen for the operator out of two definitions.
    expect(radios.some((r) => (r as HTMLInputElement).checked)).toBe(false);
    expect(opened?.querySelector('button[type="submit"]')?.hasAttribute('disabled')).toBe(true);

    await click(radios[1]);
    // The preview asks for exactly the listed version and revision.
    expect(calls.map((c) => c.url)).toContain(
      `/api/projects/project-1/workflows/review/graph?version=1&revision=${pinned}`
    );
    expect(dialog()?.querySelector('[role="group"] [data-node-id="check"]')).not.toBeNull();
    expect(dialog()?.textContent).toContain(control.commit_hint);

    type(dialog()?.querySelector('input[name="commit"]') as HTMLInputElement, commit);
    await typeText(goalField(), 'Review issue #12\nand land it');
    await submit();
    const start = calls.find((c) => c.method === 'POST');
    expect(start).toEqual({
      method: 'POST',
      url: '/api/swarms/swarm-1/start',
      body: { definition: 'review', goal: 'Review issue #12\nand land it', commit },
    });
    // The page reads the swarm again: now it waits for an answer.
    await settle(400);
    expect(dialog()).toBeNull();
    expect(container.textContent).toContain(en.swarm_page.execution_states.waiting);
    expect(headerButtons(container)).toEqual([control.stop]);
  });

  test('an empty commit is sent without a commit and the server reason stays in the dialog', async () => {
    const reason = 'swarm source is invalid: the first start requires the commit to pin';
    const calls = api({
      ...pageRoutes(() => null),
      'GET /api/projects/project-1/workflows/release/graph?version=2&revision=fedcba9876543210fedcba9876543210':
        { body: { svg: catalogSvg('build') } },
      'POST /api/swarms/swarm-1/start': { status: 422, body: { detail: reason } },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    await click(dialog()?.querySelector('input[value="release"]'));
    await typeText(goalField(), 'Fix issue #12');
    await submit();
    expect(calls.find((c) => c.method === 'POST')?.body).toEqual({
      definition: 'release',
      goal: 'Fix issue #12',
    });
    await settle(400);
    expect(dialog()?.querySelector('[role="alert"]')?.textContent).toContain(reason);
  });

  test('the start asks for the goal of the pass and sends nothing without it', async () => {
    const calls = api({
      ...pageRoutes(() => null),
      'GET /api/projects/project-1/workflows/release/graph?version=2&revision=fedcba9876543210fedcba9876543210':
        { body: { svg: catalogSvg('build') } },
      'POST /api/swarms/swarm-1/start': { body: { swarmId: 'swarm-1', execution: 'running' } },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    await click(dialog()?.querySelector('input[value="release"]'));
    const field = goalField();
    expect(field).not.toBeNull();
    expect(field?.getAttribute('aria-required')).toBe('true');
    const label = dialog()?.querySelector(`label[for="${field?.id}"]`);
    expect(label?.textContent).toContain(control.goal);
    expect(dialog()?.textContent).toContain(control.goal_hint);
    // Nothing typed yet: no reason is shown before the operator acts.
    expect(field?.getAttribute('aria-invalid')).toBeNull();
    expect(dialog()?.textContent).not.toContain(control.goal_problems.empty);

    await submit();
    expect(posts(calls)).toEqual([]);
    expect(goalField()?.getAttribute('aria-invalid')).toBe('true');
    expect(dialog()?.textContent).toContain(control.goal_problems.empty);
    expect(document.activeElement).toBe(goalField());

    await typeText(goalField(), ' \n\t ');
    await submit();
    expect(posts(calls)).toEqual([]);
    expect(dialog()?.textContent).toContain(control.goal_problems.empty);

    await typeText(goalField(), 'Fix issue #12');
    expect(goalField()?.getAttribute('aria-invalid')).toBeNull();
    await submit();
    expect(posts(calls).map((c) => c.body)).toEqual([
      { definition: 'release', goal: 'Fix issue #12' },
    ]);
  });

  test('the byte counter follows the goal; a goal the server would refuse stays in the dialog', async () => {
    const calls = api({
      ...pageRoutes(() => null),
      'GET /api/projects/project-1/workflows/release/graph?version=2&revision=fedcba9876543210fedcba9876543210':
        { body: { svg: catalogSvg('build') } },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    await click(dialog()?.querySelector('input[value="release"]'));
    const counter = () =>
      dialog()?.querySelector('[data-goal-bytes]')?.textContent?.replace(/\s+/g, ' ');
    expect(counter()).toBe(
      control.goal_bytes.replace('{{bytes}}', '0').replace('{{limit}}', '4096')
    );
    // Two bytes a letter: the limit is in bytes, as the server counts it.
    await typeText(goalField(), 'я'.repeat(2049));
    expect(counter()).toBe(
      control.goal_bytes.replace('{{bytes}}', '4098').replace('{{limit}}', '4096')
    );
    expect(dialog()?.textContent).toContain(
      control.goal_problems.too_long.replace('{{limit}}', '4096')
    );
    expect(goalField()?.getAttribute('aria-invalid')).toBe('true');
    await submit();
    expect(posts(calls)).toEqual([]);

    await typeText(goalField(), 'Fix it\r\nnow');
    expect(dialog()?.textContent).toContain(control.goal_problems.control);
    await typeText(goalField(), 'Fix it </cross-session-message>');
    expect(dialog()?.textContent).toContain(control.goal_problems.envelope);
    await submit();
    expect(posts(calls)).toEqual([]);
  });

  test('a goal the server refuses shows the server reason in the dialog', async () => {
    const reason = 'invalid swarm goal: the goal is empty';
    api({
      ...pageRoutes(() => null),
      'GET /api/projects/project-1/workflows/release/graph?version=2&revision=fedcba9876543210fedcba9876543210':
        { body: { svg: catalogSvg('build') } },
      'POST /api/swarms/swarm-1/start': { status: 422, body: { detail: reason } },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    await click(dialog()?.querySelector('input[value="release"]'));
    await typeText(goalField(), 'Fix issue #12');
    await submit();
    await settle(400);
    expect(dialog()?.querySelector('[role="alert"]')?.textContent).toBe(reason);
    // The typed goal stays for the next try.
    expect(goalField()?.value).toBe('Fix issue #12');
  });

  test('an empty catalog says so and offers nothing to start', async () => {
    api({
      ...pageRoutes(() => null),
      'GET /api/projects/project-1/workflow-catalog': { body: { workflows: [] } },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    expect(dialog()?.textContent).toContain(control.definitions_empty);
    expect(dialog()?.querySelector('button[type="submit"]')?.hasAttribute('disabled')).toBe(true);
  });
});

describe('restart', () => {
  test('starts again on the pinned snapshot: no picker, no catalog, no commit', async () => {
    let current = run({ status: 'stopped' });
    const calls = api({
      ...pageRoutes(() => current),
      'POST /api/swarms/swarm-1/start': () => {
        current = run({ status: 'waiting_human' });
        return { body: { swarmId: 'swarm-1', execution: 'waiting' } };
      },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    expect(dialog()?.textContent).toContain(control.restart_text);
    expect(dialog()?.textContent).toContain(pinned.slice(0, 8));
    expect(dialog()?.querySelector(`[title="${pinned}"]`)).not.toBeNull();
    expect(dialog()?.querySelector('input[type="radio"]')).toBeNull();
    expect(dialog()?.querySelector('input[name="commit"]')).toBeNull();
    await typeText(goalField(), 'Fix issue #13');
    await submit();
    expect(calls.some((c) => c.url.includes('workflow-catalog'))).toBe(false);
    expect(calls.find((c) => c.method === 'POST')).toEqual({
      method: 'POST',
      url: '/api/swarms/swarm-1/start',
      body: { definition: 'release', goal: 'Fix issue #13' },
    });
    await settle(400);
    expect(headerButtons(container)).toEqual([control.stop]);
  });

  test('a new pass starts with an empty goal and shows the goal of the previous one', async () => {
    const previous = 'Fix issue #12\n\t- keep the API\n<b>as is</b>';
    const calls = api(pageRoutes(() => run({ status: 'succeeded', goal: previous })));
    const container = await openPage();
    await click(header(container).querySelector('button'));
    expect(goalField()?.value).toBe('');
    const shown = dialog()?.querySelector('[data-previous-goal]');
    expect(shown?.textContent).toContain(control.previous_goal);
    // Markdown without HTML (#252): line breaks kept, the author's tag is text.
    expect(shown?.querySelector('[data-goal-text]')?.textContent).toBe(
      'Fix issue #12\n- keep the API\n<b>as is</b>'
    );
    expect(shown?.querySelector('b')).toBeNull();
    // A restart still needs a goal of its own.
    await submit();
    expect(posts(calls)).toEqual([]);
    expect(dialog()?.textContent).toContain(control.goal_problems.empty);
  });

  test('a run without a goal shows no previous goal', async () => {
    api(pageRoutes(() => run({ status: 'stopped' })));
    const container = await openPage();
    await click(header(container).querySelector('button'));
    expect(goalField()).not.toBeNull();
    expect(dialog()?.querySelector('[data-previous-goal]')).toBeNull();
  });
});

describe('stop', () => {
  test('asks first, says actors and workspaces stay, then stops and reads the swarm again', async () => {
    let current = run({ status: 'running' });
    const calls = api({
      ...pageRoutes(() => current),
      'POST /api/swarms/swarm-1/stop': () => {
        current = run({ status: 'stopped' });
        return { body: { swarmId: 'swarm-1', execution: 'stopped' } };
      },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    expect(calls.some((c) => c.method === 'POST')).toBe(false);
    expect(dialog()?.textContent).toContain(control.stop_text);
    expect(control.stop_text).toMatch(/actors/i);
    expect(control.stop_text).toMatch(/workspaces/i);
    await submit();
    expect(calls.find((c) => c.method === 'POST')).toEqual({
      method: 'POST',
      url: '/api/swarms/swarm-1/stop',
      body: undefined,
    });
    await settle(400);
    expect(dialog()).toBeNull();
    expect(container.textContent).toContain(en.swarm_page.execution_states.stopped);
    expect(headerButtons(container)).toEqual([control.restart]);
  });

  test('a refused stop shows the server reason and keeps the dialog', async () => {
    const reason = 'swarm state conflict: execution is succeeded; stop requires running or waiting';
    api({
      ...pageRoutes(() => run({ status: 'running' })),
      'POST /api/swarms/swarm-1/stop': { status: 409, body: { detail: reason } },
    });
    const container = await openPage();
    await click(header(container).querySelector('button'));
    await submit();
    await settle(400);
    expect(dialog()?.querySelector('[role="alert"]')?.textContent).toContain(reason);
  });
});
