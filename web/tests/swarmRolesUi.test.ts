// FEATURE: Bind swarm roles from the swarm page (#241, API #235).
// SPEC: The swarm page carries a role block: every role of the graph (the
// snapshot, or before the first start the definition chosen in the start
// dialog) and every stored binding, with its state in words and its actor
// by name. A manager of the active swarm picks one of this swarm's running
// managed actors and binds the role with the incarnation the actor list
// showed, binds it again or unbinds it; the server's answer is the new
// view, and a refusal stays at the role with the server's reason. While
// the execution runs or waits the block says why nothing changes instead
// of offering dead controls, and a viewer reads without controls. The start
// dialog names the roles that are not ready before the start is pressed.
import { afterAll, afterEach, describe, expect, test } from 'bun:test';
import { act, createElement, type ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun } from '@/services/brood/pipelines';
import type { GraphRole, SwarmRolesView } from '@/services/brood/roles';
import { RolesSection } from '@/features/swarms/SwarmRoles';
import type { SwarmRolesModel } from '@/features/swarms/useSwarmRoles';
import { SwarmPage } from '@/features/swarms/SwarmPage';
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

const t = en.swarm_roles;
const revision = '0123456789abcdef0123456789abcdef';
const fill = (text: string, values: Record<string, string | number>) =>
  text.replace(/\{\{(\w+)\}\}/g, (_, key: string) => String(values[key]));

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

const run = (over: Partial<PipelineRun> = {}): PipelineRun => ({
  id: 'swarm-1',
  name: 'release',
  definition: { name: 'release', version: 1, revision },
  status: 'stopped',
  swarmId: 'swarm-1',
  projectId: 'project-1',
  projectName: 'brood',
  startedAt: null,
  currentNode: 'build',
  nodes: [{ id: 'build', kind: 'actor', label: 'build', status: 'done', sequence: 1 }],
  ...over,
});

const ready = (over: Partial<GraphRole> = {}): GraphRole => ({
  role: 'builder',
  inGraph: true,
  state: 'ready',
  actorUid: 'uid-b1',
  actorName: 'builder-1',
  actorIncarnation: 2,
  ...over,
});
const unbound = (name: string): GraphRole => ({ role: name, inGraph: true, state: 'unbound' });

const rolesView = (over: Partial<SwarmRolesView> = {}): SwarmRolesView => ({
  swarmId: 'swarm-1',
  execution: 'stopped',
  graph: { pinned: true, name: 'release', version: 1, revision },
  roles: [ready(), unbound('reviewer')],
  ...over,
});

const managed = (name: string, uid: string, generation: string, phase = 'running') => ({
  metadata: { name, uid },
  provider: 'proxmox',
  generation,
  phase,
  desired: phase === 'stopped' ? 'stopped' : 'running',
});
const actorList = {
  results: [
    managed('builder-1', 'uid-b1', '2'),
    managed('builder-2', 'uid-b2', '1'),
    managed('parked', 'uid-p', '4', 'stopped'),
    { metadata: { name: 'native-1', uid: 'uid-n' }, status: { state: 'RUNNING' } },
  ],
};

// ---------------------------------------------------------------- static

const model = (over: Partial<SwarmRolesModel> = {}): SwarmRolesModel => ({
  view: rolesView(),
  error: '',
  refresh: async () => {},
  definition: '',
  control: 'edit',
  actors: [
    { uid: 'uid-b1', name: 'builder-1', incarnation: 2, phase: 'running', desired: 'running' },
    { uid: 'uid-b2', name: 'builder-2', incarnation: 1, phase: 'running', desired: 'running' },
  ],
  actorsError: '',
  refreshActors: async () => {},
  notices: {},
  pending: null,
  focus: null,
  focused: () => {},
  bind: async () => {},
  unbind: async () => {},
  ...over,
});

const wrap = (node: ReactNode) =>
  createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null, node));
const markup = (over: Partial<SwarmRolesModel> = {}) =>
  renderToStaticMarkup(wrap(createElement(RolesSection, { model: model(over) })));
const visibleText = (html: string) =>
  html
    .replace(/<[^>]*>/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
/** Разметка одной роли: от её строки до следующей. */
const row = (html: string, name: string) => {
  const start = html.indexOf(`data-role="${name}"`);
  if (start < 0) throw new Error(`no row for ${name}`);
  const next = html.indexOf('data-role="', start + 1);
  return html.slice(start, next < 0 ? undefined : next);
};

describe('what each role shows', () => {
  test('every role has its state in words and its actor by name', () => {
    const html = markup({
      view: rolesView({
        roles: [
          ready(),
          { role: 'gone', inGraph: true, state: 'missing', actorUid: 'uid-g', actorIncarnation: 1 },
          {
            role: 'other',
            inGraph: true,
            state: 'foreign',
            actorUid: 'uid-o',
            actorIncarnation: 1,
          },
          unbound('reviewer'),
          ready({ role: 'tester', state: 'stale', actorIncarnation: 1 }),
          ready({ role: 'writer', state: 'inactive' }),
        ],
      }),
    });
    const text = (name: string) => visibleText(row(html, name));
    expect(text('builder')).toContain(t.states.ready);
    expect(text('builder')).toContain('builder-1');
    expect(text('reviewer')).toContain(t.states.unbound);
    expect(text('reviewer')).toContain(t.actor_none);
    expect(text('tester')).toContain(t.states.stale);
    expect(text('tester')).toContain(t.state_hints.stale);
    expect(text('writer')).toContain(t.states.inactive);
    expect(text('writer')).toContain(t.state_hints.inactive);
    // An actor of another swarm keeps its name hidden; a gone one says so.
    expect(text('other')).toContain(t.states.foreign);
    expect(text('other')).toContain(t.actor_hidden);
    expect(text('gone')).toContain(t.states.missing);
    expect(text('gone')).toContain(t.actor_gone);
  });

  test('a binding the graph does not name is marked as such', () => {
    const html = markup({
      view: rolesView({
        roles: [ready({ role: 'old', inGraph: false })],
      }),
    });
    expect(visibleText(row(html, 'old'))).toContain(t.not_in_graph);
  });

  test('the block says which graph its roles come from', () => {
    expect(visibleText(markup())).toContain(fill(t.graph_pinned, { name: 'release', version: 1 }));
    const chosen = markup({
      definition: 'review',
      view: rolesView({ graph: { pinned: false, name: 'review', version: 3, revision } }),
    });
    expect(visibleText(chosen)).toContain(fill(t.graph_definition, { name: 'review', version: 3 }));
    const none = markup({ view: rolesView({ graph: null, roles: [] }) });
    expect(visibleText(none)).toContain(t.graph_none);
    expect(visibleText(none)).toContain(t.empty_no_graph);
    // Without a graph every binding is outside it: the mark would be noise.
    const leftover = markup({ view: rolesView({ graph: null, roles: [ready()] }) });
    expect(visibleText(leftover)).not.toContain(t.not_in_graph);
  });

  test('a read failure shows its reason with a retry', () => {
    const html = markup({ view: null, error: 'swarm not found' });
    expect(html).toContain('role="alert"');
    expect(visibleText(html)).toContain('swarm not found');
  });

  // #253: the block is a fold; it stands open only when a role needs a hand
  // or the swarm is not working, and its summary counts what is not ready.
  test('the block folds while the swarm works with every role ready, and opens otherwise', () => {
    const folded = markup({
      control: 'busy',
      view: rolesView({ execution: 'running', roles: [ready()] }),
    });
    expect(folded).toMatch(/<details(?![^>]*\sopen)[^>]*>/);
    expect(folded.slice(0, folded.indexOf('</summary>'))).toContain(
      fill(t.fold_ready, { count: 1 })
    );
    const unbound = markup({ control: 'busy', view: rolesView({ execution: 'running' }) });
    expect(unbound).toMatch(/<details[^>]*\sopen/);
    expect(unbound.slice(0, unbound.indexOf('</summary>'))).toContain(
      fill(t.fold_unready, { count: 1 })
    );
    expect(markup()).toMatch(/<details[^>]*\sopen/);
    // The title stays a heading the page outline names.
    expect(markup()).toMatch(new RegExp(`<h[2-4][^>]*>${t.title}</h[2-4]>`));
  });
});

describe('who gets controls', () => {
  test('a manager gets an actor choice, bind and unbind named by role', () => {
    const html = markup();
    const builder = row(html, 'builder');
    expect(builder).toContain(`aria-label="${fill(t.choose, { role: 'builder' })}"`);
    expect(builder).toContain(`aria-label="${fill(t.rebind_role, { role: 'builder' })}"`);
    expect(builder).toContain(`aria-label="${fill(t.unbind_role, { role: 'builder' })}"`);
    const reviewer = row(html, 'reviewer');
    expect(reviewer).toContain(`aria-label="${fill(t.bind_role, { role: 'reviewer' })}"`);
    // An unbound role has nothing to unbind.
    expect(reviewer).not.toContain(fill(t.unbind_role, { role: 'reviewer' }));
  });

  test('a running or waiting swarm says why bindings stay, with no controls at all', () => {
    const html = markup({ control: 'busy', view: rolesView({ execution: 'running' }) });
    expect(visibleText(html)).toContain(t.busy);
    expect(html).not.toContain('<button');
    expect(html).not.toContain('aria-haspopup="listbox"');
    expect(visibleText(html)).toContain('builder-1');
  });

  test('a viewer reads the roles without controls', () => {
    const html = markup({ control: 'read' });
    expect(visibleText(html)).toContain(t.read_only);
    expect(html).not.toContain('<button');
    expect(visibleText(row(html, 'builder'))).toContain(t.states.ready);
  });

  test('no running actor to offer: the block says where actors are made, unbind stays', () => {
    const html = markup({ actors: [] });
    expect(visibleText(html)).toContain(t.actors_none);
    expect(html).toContain('href="/actors"');
    expect(row(html, 'builder')).toContain(fill(t.unbind_role, { role: 'builder' }));
    expect(html).not.toContain('aria-haspopup="listbox"');
  });

  test('which actors can take a role is said only when there is a role to bind', () => {
    expect(visibleText(markup())).toContain(t.actors_hint);
    expect(visibleText(markup({ view: rolesView({ graph: null, roles: [] }) }))).not.toContain(
      t.actors_hint
    );
  });

  test('an unreadable actor list shows its reason; the roles stay readable', () => {
    const html = markup({ actors: null, actorsError: 'Substrate is unavailable' });
    expect(visibleText(html)).toContain(
      fill(t.actors_error, { reason: 'Substrate is unavailable' })
    );
    expect(visibleText(row(html, 'builder'))).toContain('builder-1');
  });

  test('a refusal stays at its role with the server reason', () => {
    const reason = 'actor is not an actor of this swarm';
    const html = markup({ notices: { reviewer: { refusal: reason } } });
    const reviewer = row(html, 'reviewer');
    expect(reviewer).toContain('role="alert"');
    expect(visibleText(reviewer)).toContain(reason);
    expect(row(html, 'builder')).not.toContain('role="alert"');
  });
});

// ---------------------------------------------------------------- live

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

async function openPage() {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  document.cookie = 'csrf=token-1';
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
    )
  );
  await settle();
  await settle();
  return container;
}

async function click(element: Element | null | undefined) {
  if (!element) throw new Error('nothing to click');
  await act(async () => (element as HTMLElement).click());
  await settle();
  await settle();
}

const rolesBlock = (container: HTMLElement) => {
  // #256: секция страницы — h2, вложенная — h3; блок ищется по заголовку любого уровня.
  const title = Array.from(container.querySelectorAll('h2, h3')).find(
    (heading) => heading.textContent === t.title
  );
  const section = title?.closest('section');
  if (!section) throw new Error('no role block');
  return section as HTMLElement;
};
const roleRow = (container: HTMLElement, name: string) => {
  const found = rolesBlock(container).querySelector(`[data-role="${name}"]`);
  if (!found) throw new Error(`no row for ${name}`);
  return found as HTMLElement;
};
const buttonLabelled = (scope: Element, label: string) =>
  scope.querySelector(`button[aria-label="${label}"]`) as HTMLButtonElement | null;
const dialog = () => document.querySelector('[role="dialog"]') as HTMLElement | null;

/** Выбор актора общим Select: открыть список и нажать вариант. */
async function choose(scope: Element, role: string, option: string) {
  await click(buttonLabelled(scope, fill(t.choose, { role })));
  const item = Array.from(document.querySelectorAll('[role="option"]')).find(
    (candidate) => candidate.textContent === option
  );
  await click(item);
}

const option = (name: string, phase: 'running' | 'provisioning' = 'running') =>
  fill(t.actor_option, { name, phase: t.phases[phase] });

const pageRoutes = ({
  current = () => run() as PipelineRun | null,
  roles = () => rolesView() as SwarmRolesView,
  over = {} as Partial<AccessSwarm>,
} = {}) => ({
  'GET /api/swarms/swarm-1': { body: swarm(over) },
  'GET /api/projects': { body: [{ id: 'project-1', name: 'brood' }] },
  'GET /api/pipeline-runs/swarm-1': () => {
    const value = current();
    return value ? { body: value } : { status: 404, body: { detail: 'pipeline run not found' } };
  },
  'GET /api/pipeline-runs/swarm-1/graph': { body: { svg: '<svg></svg>' } },
  'GET /api/swarms/swarm-1/requests?all=1': { body: { results: [] } },
  'GET /api/swarms/swarm-1/roles': () => ({ body: roles() }),
  'GET /api/swarms/swarm-1/actors': { body: actorList },
});

describe('binding on the page', () => {
  test('binds an unbound role to the chosen running actor with its incarnation', async () => {
    let roles = rolesView();
    const calls = api({
      ...pageRoutes({ roles: () => roles }),
      'PUT /api/swarms/swarm-1/roles/reviewer': () => {
        roles = rolesView({
          roles: [
            ready(),
            ready({
              role: 'reviewer',
              actorUid: 'uid-b2',
              actorName: 'builder-2',
              actorIncarnation: 1,
            }),
          ],
        });
        return { body: roles };
      },
    });
    const container = await openPage();
    const reviewer = roleRow(container, 'reviewer');
    expect(reviewer.textContent).toContain(t.states.unbound);
    await click(buttonLabelled(reviewer, fill(t.choose, { role: 'reviewer' })));
    // Only running managed actors of this swarm are offered.
    expect(
      Array.from(document.querySelectorAll('[role="option"]')).map((o) => o.textContent)
    ).toEqual([option('builder-1'), option('builder-2')]);
    await click(
      Array.from(document.querySelectorAll('[role="option"]')).find(
        (o) => o.textContent === option('builder-2')
      )
    );
    await click(
      buttonLabelled(roleRow(container, 'reviewer'), fill(t.bind_role, { role: 'reviewer' }))
    );
    expect(calls.filter((c) => c.method === 'PUT')).toEqual([
      {
        method: 'PUT',
        url: '/api/swarms/swarm-1/roles/reviewer',
        body: { actorUid: 'uid-b2', actorIncarnation: 1 },
      },
    ]);
    const after = roleRow(container, 'reviewer');
    expect(after.textContent).toContain(t.states.ready);
    expect(after.textContent).toContain('builder-2');
    const status = after.querySelector('[role="status"]');
    expect(status?.textContent).toBe(fill(t.bound, { role: 'reviewer', actor: 'builder-2' }));
    // The pressed button was busy during the write and lost focus: the result takes it.
    expect(document.activeElement).toBe(status);
  });

  test('rebinds a bound role to another actor of the swarm', async () => {
    const calls = api({
      ...pageRoutes(),
      'PUT /api/swarms/swarm-1/roles/builder': {
        body: rolesView({
          roles: [
            ready({ actorUid: 'uid-b2', actorName: 'builder-2', actorIncarnation: 1 }),
            unbound('reviewer'),
          ],
        }),
      },
    });
    const container = await openPage();
    const builder = roleRow(container, 'builder');
    // The bound actor is the starting choice.
    expect(buttonLabelled(builder, fill(t.choose, { role: 'builder' }))?.textContent).toBe(
      option('builder-1')
    );
    await choose(builder, 'builder', option('builder-2'));
    await click(
      buttonLabelled(roleRow(container, 'builder'), fill(t.rebind_role, { role: 'builder' }))
    );
    expect(calls.find((c) => c.method === 'PUT')).toEqual({
      method: 'PUT',
      url: '/api/swarms/swarm-1/roles/builder',
      body: { actorUid: 'uid-b2', actorIncarnation: 1 },
    });
    expect(roleRow(container, 'builder').textContent).toContain('builder-2');
  });

  test('a stale binding is renewed with the incarnation the actor has now', async () => {
    const calls = api({
      ...pageRoutes({
        roles: () => rolesView({ roles: [ready({ state: 'stale', actorIncarnation: 1 })] }),
      }),
      'PUT /api/swarms/swarm-1/roles/builder': { body: rolesView({ roles: [ready()] }) },
    });
    const container = await openPage();
    const builder = roleRow(container, 'builder');
    expect(builder.textContent).toContain(t.states.stale);
    await click(buttonLabelled(builder, fill(t.rebind_role, { role: 'builder' })));
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({
      actorUid: 'uid-b1',
      actorIncarnation: 2,
    });
    expect(roleRow(container, 'builder').textContent).toContain(t.states.ready);
  });

  test('unbinds a role, and focus lands on the result rather than nowhere', async () => {
    const calls = api({
      ...pageRoutes(),
      'DELETE /api/swarms/swarm-1/roles/builder': {
        body: rolesView({ roles: [unbound('builder'), unbound('reviewer')] }),
      },
    });
    const container = await openPage();
    await click(
      buttonLabelled(roleRow(container, 'builder'), fill(t.unbind_role, { role: 'builder' }))
    );
    expect(calls.filter((c) => c.method === 'DELETE').map((c) => c.url)).toEqual([
      '/api/swarms/swarm-1/roles/builder',
    ]);
    const builder = roleRow(container, 'builder');
    expect(builder.textContent).toContain(t.states.unbound);
    expect(buttonLabelled(builder, fill(t.unbind_role, { role: 'builder' }))).toBeNull();
    const status = builder.querySelector('[role="status"]');
    expect(status?.textContent).toBe(fill(t.unbound_done, { role: 'builder' }));
    expect(document.activeElement).toBe(status);
  });

  test('unbinding a role no graph names drops its row, and the result stays in the block', async () => {
    api({
      ...pageRoutes({
        current: () => null,
        roles: () => rolesView({ execution: 'idle', graph: null, roles: [ready({ role: 'old' })] }),
      }),
      'DELETE /api/swarms/swarm-1/roles/old': {
        body: rolesView({ execution: 'idle', graph: null, roles: [] }),
      },
    });
    const container = await openPage();
    await click(buttonLabelled(roleRow(container, 'old'), fill(t.unbind_role, { role: 'old' })));
    const block = rolesBlock(container);
    expect(block.querySelector('[data-role="old"]')).toBeNull();
    const status = block.querySelector('[role="status"]');
    expect(status?.textContent).toBe(fill(t.unbound_done, { role: 'old' }));
    expect(document.activeElement).toBe(status);
  });

  test('a refusal shows the server reason at the role and reads the actors again', async () => {
    const reason = 'actor incarnation changed; list the swarm actors and bind again';
    const calls = api({
      ...pageRoutes(),
      'PUT /api/swarms/swarm-1/roles/builder': { status: 409, body: { detail: reason } },
    });
    const container = await openPage();
    const actorReads = () => calls.filter((c) => c.url === '/api/swarms/swarm-1/actors').length;
    const before = actorReads();
    await click(
      buttonLabelled(roleRow(container, 'builder'), fill(t.rebind_role, { role: 'builder' }))
    );
    await settle();
    const alert = roleRow(container, 'builder').querySelector('[role="alert"]');
    expect(alert?.textContent).toContain(reason);
    expect(document.activeElement).toBe(alert);
    expect(actorReads()).toBeGreaterThan(before);
  });

  test('a running swarm explains that bindings wait for a stop and offers no control', async () => {
    const calls = api(
      pageRoutes({
        current: () => run({ status: 'running' }),
        roles: () => rolesView({ execution: 'running' }),
      })
    );
    const container = await openPage();
    const block = rolesBlock(container);
    expect(block.textContent).toContain(t.busy);
    expect(block.querySelector('button')).toBeNull();
    expect(block.textContent).toContain('builder-1');
    expect(calls.some((c) => c.url === '/api/swarms/swarm-1/actors')).toBe(false);
  });

  test('a viewer reads the roles and never lists actors to bind', async () => {
    const calls = api(pageRoutes({ over: { role: 'viewer' } }));
    const container = await openPage();
    const block = rolesBlock(container);
    expect(block.textContent).toContain(t.read_only);
    expect(block.querySelector('button')).toBeNull();
    expect(calls.some((c) => c.url === '/api/swarms/swarm-1/actors')).toBe(false);
  });
});

describe('before the first start', () => {
  const catalog = {
    workflows: [
      { name: 'release', version: 2, revision, publishedAt: null },
      { name: 'review', version: 3, revision, publishedAt: null },
    ],
  };
  const reviewRoles = rolesView({
    execution: 'idle',
    graph: { pinned: false, name: 'review', version: 3, revision },
    roles: [ready(), unbound('reviewer')],
  });

  test('the block follows the definition chosen in the start dialog, which warns before the start', async () => {
    const calls = api({
      ...pageRoutes({
        current: () => null,
        roles: () => rolesView({ execution: 'idle', graph: null, roles: [] }),
      }),
      'GET /api/projects/project-1/workflow-catalog': { body: catalog },
      [`GET /api/projects/project-1/workflows/review/graph?version=3&revision=${revision}`]: {
        body: { svg: '<svg></svg>' },
      },
      'GET /api/swarms/swarm-1/roles?definition=review': { body: reviewRoles },
    });
    const container = await openPage();
    expect(rolesBlock(container).textContent).toContain(t.graph_none);
    const start = container.querySelector('[data-swarm-action] button');
    await click(start);
    expect(calls.some((c) => c.url.includes('?definition='))).toBe(false);
    await click(dialog()?.querySelector('input[value="review"]'));
    await settle();
    expect(calls.map((c) => c.url)).toContain('/api/swarms/swarm-1/roles?definition=review');
    const note = dialog()?.querySelector('[data-roles-note]') as HTMLElement | null;
    expect(note?.textContent).toContain(t.start.unready);
    expect(note?.textContent).toContain('reviewer');
    expect(note?.textContent).toContain(t.states.unbound);
    expect(note?.textContent).not.toContain('builder');
    // The note describes the start button: it is read before the press.
    const submit = dialog()?.querySelector('button[type="submit"]');
    expect(submit?.getAttribute('aria-describedby')).toBe(note?.id ?? 'missing');
    expect(calls.some((c) => c.method === 'POST')).toBe(false);
    // Behind the dialog the block already shows the roles of that definition.
    expect(rolesBlock(container).textContent).toContain(
      fill(t.graph_definition, { name: 'review', version: 3 })
    );
    expect(roleRow(container, 'reviewer').textContent).toContain(t.states.unbound);
  });

  test('a graph whose roles are all ready says so in the dialog', async () => {
    api({
      ...pageRoutes({ roles: () => rolesView({ roles: [ready()] }) }),
    });
    const container = await openPage();
    await click(container.querySelector('[data-swarm-action] button'));
    const note = dialog()?.querySelector('[data-roles-note]');
    expect(note?.textContent).toContain(t.start.ready);
  });

  test('the restart dialog names a stale role of the snapshot', async () => {
    api(
      pageRoutes({
        roles: () => rolesView({ roles: [ready({ state: 'stale', actorIncarnation: 1 })] }),
      })
    );
    const container = await openPage();
    await click(container.querySelector('[data-swarm-action] button'));
    const note = dialog()?.querySelector('[data-roles-note]');
    expect(note?.textContent).toContain('builder');
    expect(note?.textContent).toContain(t.states.stale);
  });
});
