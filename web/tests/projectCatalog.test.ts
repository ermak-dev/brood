// FEATURE: The workflow catalog of a project on its page, for any member
// (#240, catalog #199, first start #218, epic #204).
// SPEC: The project page lists the published definitions of the project to
// anyone who may read the project, administrator or not: name, version,
// short revision (whole in the hint) and publication time. The first one is
// shown until another is chosen; a choice draws the graph of exactly the
// listed version and revision with the graph component of the swarm page,
// read-only. An empty catalog says that `bro build` publishes definitions from
// the Broodfile. A refusal of the catalog or of a graph shows the server's
// reason, and a graph refused because the catalog moved on reads the catalog
// again. The definition shown lives in the address (#270):
// /projects/P?workflow=W opens W, a choice writes its name there in place of
// the current entry, back and forward show the definition of their address,
// and a name the catalog lacks is named with an action that clears it rather
// than replaced by the first.
import { afterAll, afterEach, describe, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, useLocation, useNavigate } from 'react-router-dom';
import type { WorkflowDefinition } from '@/services/brood/pipelines';
import { catalogSelection, definitionKey } from '@/features/pipelines/catalog';
import { ProjectDetailPage } from '@/features/access/ProjectDetailPage';
import { useAuthStore, useProjectStore } from '@/stores';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
// Хранилища выбора проекта и сессии пишут в localStorage: на время файла — память.
const storage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
const memory = new Map<string, string>();
Object.defineProperty(globalThis, 'localStorage', {
  configurable: true,
  value: {
    getItem: (key: string) => memory.get(key) ?? null,
    setItem: (key: string, value: string) => memory.set(key, value),
    removeItem: (key: string) => memory.delete(key),
  },
});
afterAll(() => {
  restoreDOM();
  if (storage) Object.defineProperty(globalThis, 'localStorage', storage);
  else Reflect.deleteProperty(globalThis, 'localStorage');
});

const i18n = createInstance();
await i18n.init({
  lng: 'en',
  resources: { en: { translation: en } },
  interpolation: { escapeValue: false },
});

const catalogText = en.pipelines.catalog;
const first = '5a43dbe2a09507ae06335bce08f5349e';
const second = 'f3faeadd4d51673355e2d20a2fb57628';
const third = '0123456789abcdef0123456789abcdef';

const definition = (over: Partial<WorkflowDefinition> = {}): WorkflowDefinition => ({
  name: 'release',
  version: 1,
  revision: first,
  publishedAt: '2026-11-10T15:34:08Z',
  ...over,
});

describe('which definition the page shows', () => {
  const release = definition();
  const architecture = definition({ name: 'architecture', version: 2, revision: second });

  test('nothing before the catalog is read and nothing in an empty one', () => {
    expect(catalogSelection(null, undefined)).toEqual({ shown: undefined, missing: false });
    expect(catalogSelection(null, 'release')).toEqual({ shown: undefined, missing: false });
    expect(catalogSelection([], undefined)).toEqual({ shown: undefined, missing: false });
  });

  test('the first definition while the address names none', () => {
    expect(catalogSelection([architecture, release], undefined)).toEqual({
      shown: architecture,
      missing: false,
    });
  });

  test('the definition the address names, in its current catalog version', () => {
    const newer = definition({ version: 3, revision: third });
    expect(catalogSelection([architecture, release], 'release')).toEqual({
      shown: release,
      missing: false,
    });
    expect(catalogSelection([architecture, newer], 'release')).toEqual({
      shown: newer,
      missing: false,
    });
  });

  // Правило #194: адрес, назвавший то, чего нет, не подменяется первым.
  test('a definition the address names but the catalog lacks is missing, not the first', () => {
    expect(catalogSelection([architecture], 'release')).toEqual({
      shown: undefined,
      missing: true,
    });
    expect(catalogSelection([], 'release')).toEqual({ shown: undefined, missing: true });
  });

  test('names are compared exactly', () => {
    expect(catalogSelection([architecture, release], 'Release').missing).toBe(true);
    expect(catalogSelection([architecture, release], ' release').missing).toBe(true);
  });

  test('a definition is identified by name, version and revision together', () => {
    expect(definitionKey(release)).toBe(`release:1:${first}`);
    expect(definitionKey(definition({ revision: third }))).not.toBe(definitionKey(release));
    expect(definitionKey(definition({ version: 2 }))).not.toBe(definitionKey(release));
  });
});

const graphSvg = (node: string) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 80">
<g class="node node-actor" data-node-id="${node}"><rect x="0" y="0" width="50" height="20"/><text x="25" y="14">${node}</text></g>
</svg>`;

const graphPath = (name: string, version: number, revision: string) =>
  `/api/projects/project-1/workflows/${name}/graph?version=${version}&revision=${revision}`;

type Reply = { status?: number; body: unknown };
type Call = { method: string; url: string };

/** Go API страницы проекта: ответы по методу и адресу, вызовы записаны. */
function api(routes: Record<string, Reply | (() => Reply)>) {
  const calls: Call[] = [];
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    const call = { method: init?.method ?? 'GET', url };
    calls.push(call);
    const route = routes[`${call.method} ${url}`];
    const reply = typeof route === 'function' ? route() : route;
    const { status = 200, body } = reply ?? { status: 404, body: { detail: 'not found' } };
    return new Response(JSON.stringify(body), { status });
  }) as typeof fetch;
  return calls;
}

const projectRoutes = (catalog: Reply | (() => Reply)) => ({
  'GET /api/projects': { body: [{ id: 'project-1', name: 'brood' }] },
  'GET /api/projects/project-1': {
    body: {
      id: 'project-1',
      name: 'brood',
      capacity: { worker_selector: {} },
      usage_available: false,
    },
  },
  'GET /api/projects/project-1/repository': { body: { origin: '' } },
  'GET /api/projects/project-1/members': {
    body: [{ username: 'victor', display_name: '', is_admin: false, disabled: false }],
  },
  'GET /api/projects/project-1/workflow-catalog': catalog,
});

const published = {
  workflows: [
    definition({ name: 'architecture', version: 2, revision: second }),
    definition({ publishedAt: null }),
  ],
};

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
});

const settle = () => act(async () => new Promise((resolve) => setTimeout(resolve, 0)));

// Адрес страницы и кнопки «Назад» и «Вперёд», как у браузера.
function Address() {
  const { pathname, search } = useLocation();
  const navigate = useNavigate();
  return createElement(
    'nav',
    null,
    createElement('output', { 'data-location': `${pathname}${search}` }),
    createElement('button', { type: 'button', 'data-travel': '-1', onClick: () => navigate(-1) }),
    createElement('button', { type: 'button', 'data-travel': '1', onClick: () => navigate(1) })
  );
}
const addressOf = (container: HTMLElement) =>
  container.querySelector('[data-location]')?.getAttribute('data-location');
const travel = (container: HTMLElement, delta: -1 | 1) =>
  click(container.querySelector(`[data-travel="${delta}"]`));

/** Страница проекта глазами участника: не администратор, проект выбран.
 * entries -- история адресов, открыт последний. */
async function openProject(entries: string[] = ['/projects/project-1']) {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  useAuthStore.setState({ isSuperuser: false, username: 'victor' });
  useProjectStore.setState({ project: 'project-1', selectedFor: 'victor' });
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
          { initialEntries: entries, initialIndex: entries.length - 1 },
          createElement(ProjectDetailPage),
          createElement(Address)
        )
      )
    )
  );
  for (let i = 0; i < 4; i += 1) await settle();
  return container;
}

async function click(element: Element | null | undefined) {
  if (!element) throw new Error('nothing to click');
  await act(async () => (element as HTMLElement).click());
  for (let i = 0; i < 3; i += 1) await settle();
}

const block = (container: HTMLElement) =>
  container.querySelector('[data-catalog]') as HTMLElement | null;
const radios = (container: HTMLElement) =>
  Array.from(block(container)?.querySelectorAll('input[type="radio"]') ?? []) as HTMLInputElement[];
const row = (container: HTMLElement, name: string) =>
  block(container)?.querySelector(`[data-definition="${name}"]`) as HTMLElement | null;
const graph = (container: HTMLElement) =>
  block(container)?.querySelector('[role="group"]') as HTMLElement | null;

describe('the catalog on the project page', () => {
  test('a member who is not an administrator reads every definition with its identity', async () => {
    const calls = api({
      ...projectRoutes({ body: published }),
      [`GET ${graphPath('architecture', 2, second)}`]: { body: { svg: graphSvg('design') } },
    });
    const container = await openProject();
    expect(calls.map((c) => c.url)).toContain('/api/projects/project-1/workflow-catalog');
    expect(block(container)?.querySelector('h2')?.textContent).toBe(catalogText.title);
    expect(radios(container).map((radio) => radio.value)).toEqual(['architecture', 'release']);
    const architecture = row(container, 'architecture');
    expect(architecture?.textContent).toContain('architecture');
    expect(architecture?.textContent).toContain(catalogText.version.replace('{{version}}', '2'));
    // Ревизия короткая на экране, полная — в подсказке.
    expect(architecture?.textContent).toContain(second.slice(0, 8));
    expect(architecture?.textContent).not.toContain(second);
    expect(architecture?.querySelector(`[title*="${second}"]`)).not.toBeNull();
    const time = new Date('2026-11-10T15:34:08Z').toLocaleString('en');
    expect(architecture?.textContent).toContain(catalogText.published.replace('{{time}}', time));
    // Неизвестный момент публикации назван, а не выдуман и не пропущен.
    expect(row(container, 'release')?.textContent).toContain(catalogText.published_unknown);
  });

  test('the first definition is shown at once, by its listed version and revision', async () => {
    const calls = api({
      ...projectRoutes({ body: published }),
      [`GET ${graphPath('architecture', 2, second)}`]: { body: { svg: graphSvg('design') } },
    });
    const container = await openProject();
    expect(radios(container).map((radio) => radio.checked)).toEqual([true, false]);
    expect(calls.map((c) => c.url)).toContain(graphPath('architecture', 2, second));
    const drawn = graph(container);
    expect(drawn?.getAttribute('aria-label')).toBe(
      catalogText.graph.replace('{{name}}', 'architecture').replace('{{version}}', '2')
    );
    expect(drawn?.querySelector('[data-node-id="design"]')).not.toBeNull();
    // Граф определения только читается: его узлы не кнопки.
    expect(drawn?.querySelector('[role="button"]')).toBeNull();
    // Под графом — копирование полной ревизии показанного определения.
    expect(
      block(container)?.querySelector(`[aria-label="${catalogText.copy_revision}"]`)
    ).not.toBeNull();
  });

  test('choosing another definition draws exactly its version and revision', async () => {
    const calls = api({
      ...projectRoutes({ body: published }),
      [`GET ${graphPath('architecture', 2, second)}`]: { body: { svg: graphSvg('design') } },
      [`GET ${graphPath('release', 1, first)}`]: { body: { svg: graphSvg('ship') } },
    });
    const container = await openProject();
    await click(radios(container)[1]);
    expect(radios(container).map((radio) => radio.checked)).toEqual([false, true]);
    expect(calls.map((c) => c.url)).toContain(graphPath('release', 1, first));
    expect(graph(container)?.querySelector('[data-node-id="ship"]')).not.toBeNull();
    expect(graph(container)?.querySelector('[data-node-id="design"]')).toBeNull();
    // Граф по каталогу никогда не просится без пары version/revision.
    expect(calls.some((c) => /\/graph(\?|$)/.test(c.url) && !c.url.includes('revision='))).toBe(
      false
    );
  });

  test('an empty catalog says that bro build publishes definitions from the Broodfile', async () => {
    const calls = api(projectRoutes({ body: { workflows: [] } }));
    const container = await openProject();
    const text = block(container)?.textContent ?? '';
    expect(text).toContain(catalogText.empty);
    expect(text).toContain(catalogText.empty_hint);
    expect(catalogText.empty_hint).toContain('bro build');
    expect(catalogText.empty_hint).toContain('Broodfile');
    expect(radios(container)).toEqual([]);
    expect(calls.some((c) => c.url.includes('/graph'))).toBe(false);
  });

  test('a refused catalog shows the server reason', async () => {
    api(projectRoutes({ status: 503, body: { detail: 'workflows unavailable' } }));
    const container = await openProject();
    const alert = block(container)?.querySelector('[role="alert"]');
    expect(alert?.textContent).toContain('workflows unavailable');
    expect(radios(container)).toEqual([]);
  });

  test('a graph refused because the catalog moved on shows the reason and reads the catalog again', async () => {
    let catalog = published;
    const calls = api({
      ...projectRoutes(() => ({ body: catalog })),
      [`GET ${graphPath('architecture', 2, second)}`]: {
        status: 409,
        body: { detail: 'workflow version changed' },
      },
      [`GET ${graphPath('architecture', 3, third)}`]: { body: { svg: graphSvg('design-3') } },
    });
    const container = await openProject();
    const alert = block(container)?.querySelector('[role="alert"]');
    expect(alert?.textContent).toContain('workflow version changed');
    catalog = {
      workflows: [
        definition({ name: 'architecture', version: 3, revision: third }),
        published.workflows[1],
      ],
    };
    await click(alert?.querySelector('button'));
    const reads = calls.filter((c) => c.url.endsWith('/workflow-catalog')).length;
    expect(reads).toBe(2);
    expect(calls.map((c) => c.url)).toContain(graphPath('architecture', 3, third));
    expect(graph(container)?.querySelector('[data-node-id="design-3"]')).not.toBeNull();
    expect(row(container, 'architecture')?.textContent).toContain(third.slice(0, 8));
  });
});

const checkedNames = (container: HTMLElement) =>
  radios(container)
    .filter((radio) => radio.checked)
    .map((radio) => radio.value);
const notice = (container: HTMLElement) =>
  block(container)?.querySelector('[data-selection-notice]') as HTMLElement | null;
const graphRoutes = {
  [`GET ${graphPath('architecture', 2, second)}`]: { body: { svg: graphSvg('design') } },
  [`GET ${graphPath('release', 1, first)}`]: { body: { svg: graphSvg('ship') } },
};

describe('the definition in the address of the project page', () => {
  test('the address opens the definition it names, not the first', async () => {
    const calls = api({ ...projectRoutes({ body: published }), ...graphRoutes });
    const container = await openProject(['/projects/project-1?workflow=release']);
    expect(checkedNames(container)).toEqual(['release']);
    expect(calls.map((c) => c.url)).toContain(graphPath('release', 1, first));
    expect(calls.map((c) => c.url)).not.toContain(graphPath('architecture', 2, second));
    expect(graph(container)?.querySelector('[data-node-id="ship"]')).not.toBeNull();
    expect(notice(container)).toBeNull();
  });

  test('a choice writes its name to the address in place of the current entry', async () => {
    api({ ...projectRoutes({ body: published }), ...graphRoutes });
    const container = await openProject(['/', '/projects/project-1']);
    await click(radios(container)[1]);
    expect(addressOf(container)).toBe('/projects/project-1?workflow=release');
    expect(graph(container)?.querySelector('[data-node-id="ship"]')).not.toBeNull();
    // Стрелки по радиогруппе не копят записей: назад -- туда, откуда пришли.
    await travel(container, -1);
    expect(addressOf(container)).toBe('/');
  });

  test('back and forward show the definition of their address', async () => {
    api({ ...projectRoutes({ body: published }), ...graphRoutes });
    const container = await openProject([
      '/projects/project-1?workflow=architecture',
      '/projects/project-1?workflow=release',
    ]);
    expect(checkedNames(container)).toEqual(['release']);
    await travel(container, -1);
    expect(checkedNames(container)).toEqual(['architecture']);
    expect(graph(container)?.querySelector('[data-node-id="design"]')).not.toBeNull();
    await travel(container, 1);
    expect(checkedNames(container)).toEqual(['release']);
    expect(graph(container)?.querySelector('[data-node-id="ship"]')).not.toBeNull();
  });

  test('a name the catalog lacks is named with a way out, not replaced by the first', async () => {
    const calls = api({ ...projectRoutes({ body: published }), ...graphRoutes });
    const container = await openProject(['/projects/project-1?workflow=gone']);
    const text = notice(container)?.textContent ?? '';
    expect(text).toContain(catalogText.missing.replace('{{name}}', 'gone'));
    expect(checkedNames(container)).toEqual([]);
    expect(graph(container)).toBeNull();
    expect(calls.some((c) => c.url.includes('/graph'))).toBe(false);
    // Список остаётся: выбрать другое можно и отсюда.
    expect(radios(container).map((radio) => radio.value)).toEqual(['architecture', 'release']);
    const clear = notice(container)?.querySelector('button');
    expect(clear?.textContent).toBe(catalogText.clear_selection);
    await click(clear);
    expect(addressOf(container)).toBe('/projects/project-1');
    expect(notice(container)).toBeNull();
    expect(checkedNames(container)).toEqual(['architecture']);
    expect(calls.map((c) => c.url)).toContain(graphPath('architecture', 2, second));
  });

  test('a named definition that left the catalog on a reread is named as well', async () => {
    let catalog: { workflows: WorkflowDefinition[] } = published;
    api({
      ...projectRoutes(() => ({ body: catalog })),
      [`GET ${graphPath('release', 1, first)}`]: {
        status: 409,
        body: { detail: 'workflow version changed' },
      },
    });
    const container = await openProject(['/projects/project-1?workflow=release']);
    catalog = { workflows: [published.workflows[0]] };
    await click(block(container)?.querySelector('[role="alert"] button'));
    expect(notice(container)?.textContent).toContain(
      catalogText.missing.replace('{{name}}', 'release')
    );
    expect(checkedNames(container)).toEqual([]);
  });

  test('an empty catalog says so whatever the address names', async () => {
    api(projectRoutes({ body: { workflows: [] } }));
    const container = await openProject(['/projects/project-1?workflow=gone']);
    expect(block(container)?.textContent).toContain(catalogText.empty);
    expect(notice(container)).toBeNull();
  });
});
