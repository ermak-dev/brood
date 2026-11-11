// FEATURE: The project overview is the start page (#197, epic #190).
// SPEC: "/" opens the overview of the active project to a member and an
// administrator: repository, capacity, members, the swarms with what each is
// doing now (execution, open questions, undelivered answers) as links, and the
// workflow catalog. It reads a fixed set of project requests and polls only
// the swarm list: no request per swarm. A tile of what waits for the operator
// leads to the swarm where to act. Without an active project the page lets
// the user choose one, or says how to get one. The gateway statistics stay at
// /dashboard, an administrator page.
import { afterAll, afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { readFileSync } from 'node:fs';
import type { AccessSwarm } from '@/services/brood/access';
import { ProjectOverviewPage } from '@/features/overview/ProjectOverviewPage';
import { visibleAccessPath } from '@/features/access/permissions';
import { tickSwarmPolling } from '@/features/swarms/useSwarmPolling';
import { useAuthStore, useProjectStore, useSwarmActivityStore } from '@/stores';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
// Хранилища сессии и проекта пишут в localStorage: на время файла -- память.
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

const swarm = (id: string, name: string, fields: Partial<AccessSwarm> = {}) => ({
  id,
  project_id: 'p1',
  owner_id: 'u1',
  owner_username: 'alice',
  role: 'owner',
  name,
  atespace_name: `space-${id}`,
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-01T09:00:00Z',
  execution: 'idle',
  openRequests: 0,
  undeliveredAnswers: 0,
  ...fields,
});

const capacity = {
  worker_selector: {},
  max_swarms: '10',
  max_actors: null,
  max_cpu_millicores: '8000',
  max_memory_bytes: null,
};

let swarms: unknown[] = [];
let projects: unknown[] = [];
let catalog: unknown = {};
const API = (): Record<string, unknown> => ({
  '/api/projects': projects,
  '/api/projects/p1': {
    id: 'p1',
    name: 'brood',
    capacity,
    usage: { swarms: '3', actors: '5', cpu_millicores: '2500', memory_bytes: '8589934592' },
    usage_available: true,
  },
  '/api/projects/p1/repository': { origin: 'https://git.example/brood.git' },
  '/api/projects/p1/members': [
    { username: 'alice', display_name: 'Alice', is_admin: false, disabled: false },
    { username: 'bob', display_name: '', is_admin: false, disabled: false },
  ],
  '/api/projects/p1/swarms': swarms,
  '/api/projects/p1/workflow-catalog': catalog,
  '/api/projects/p2': {
    id: 'p2',
    name: 'other',
    capacity,
    usage: null,
    usage_available: false,
    usage_error: 'inventory unavailable',
  },
  '/api/projects/p2/repository': { origin: '' },
  '/api/projects/p2/members': [],
  '/api/projects/p2/swarms': [],
  '/api/projects/p2/workflow-catalog': { workflows: [] },
});

let requested: string[] = [];
const originalFetch = globalThis.fetch;
beforeEach(() => {
  requested = [];
  projects = [
    { id: 'p1', name: 'brood', swarmCount: 3 },
    { id: 'p2', name: 'other', swarmCount: 0 },
  ];
  swarms = [
    swarm('s-idle', 'idle-crew'),
    swarm('s-busy', 'busy-crew', { execution: 'running' }),
    swarm('s-gate', 'gate-crew', { execution: 'waiting', openRequests: 2 }),
    swarm('s-done', 'done-crew', { execution: 'succeeded', undeliveredAnswers: 1 }),
  ];
  catalog = {
    workflows: [
      {
        name: 'review',
        version: 2,
        revision: '0123456789abcdef0123456789abcdef',
        publishedAt: '2026-11-05T10:00:00Z',
      },
    ],
  };
  globalThis.fetch = (async (input: string | URL | Request) => {
    const url = String(input);
    requested.push(url);
    const body = API()[url];
    return body === undefined
      ? new Response(JSON.stringify({ detail: 'not found' }), { status: 404 })
      : new Response(JSON.stringify(body), { status: 200 });
  }) as typeof fetch;
  useProjectStore.setState({ project: 'p1', selectedFor: 'alice' });
  useAuthStore.setState({ isSuperuser: false });
});
afterEach(() => {
  globalThis.fetch = originalFetch;
  useAuthStore.setState({ isSuperuser: false });
  for (const node of Array.from(document.body.children)) node.remove();
});

function Address() {
  const { pathname, search } = useLocation();
  return createElement('output', { 'data-location': `${pathname}${search}` });
}

const settle = () => act(async () => new Promise((resolve) => setTimeout(resolve, 0)));

interface Mounted {
  root: Root;
  container: HTMLElement;
}

async function mount(entry: string): Promise<Mounted> {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  const page = createElement(
    Routes,
    null,
    createElement(Route, { path: '/', element: createElement(ProjectOverviewPage) })
  );
  await act(async () =>
    root.render(
      createElement(
        I18nextProvider,
        { i18n },
        createElement(MemoryRouter, { initialEntries: [entry] }, page, createElement(Address))
      )
    )
  );
  for (let i = 0; i < 4; i += 1) await settle();
  return { root, container };
}

async function unmount({ root, container }: Mounted) {
  await act(async () => root.unmount());
  container.remove();
}

const addressOf = (mounted: Mounted) =>
  mounted.container.querySelector('output')?.getAttribute('data-location');
const text = (mounted: Mounted) => mounted.container.textContent ?? '';
const link = (mounted: Mounted, href: string) =>
  mounted.container.querySelector<HTMLAnchorElement>(`a[href="${href}"]`);
const section = (mounted: Mounted, name: string) =>
  mounted.container.querySelector<HTMLElement>(`[data-overview="${name}"]`);

describe('the overview of the active project', () => {
  test('a member sees the repository, capacity, members, swarms and workflows', async () => {
    const page = await mount('/');
    expect(page.container.querySelector('h1')?.textContent).toBe('brood');
    expect(section(page, 'repository')?.textContent).toContain('https://git.example/brood.git');
    const capacityText = section(page, 'capacity')?.textContent ?? '';
    expect(capacityText).toContain('3 of 10');
    expect(capacityText).toContain('5, no limit');
    expect(capacityText).toContain('2.5 CPU of 8 CPU');
    expect(capacityText).toContain('8 GiB, no limit');
    const membersText = section(page, 'members')?.textContent ?? '';
    expect(membersText).toContain('Alice');
    expect(membersText).toContain('bob');
    const workflows = section(page, 'workflows');
    expect(workflows?.textContent).toContain('review');
    expect(workflows?.textContent).toContain('version 2');
    expect(workflows?.querySelector('a[href="/projects/p1"]')).not.toBeNull();
    expect(link(page, '/projects/p1')).not.toBeNull();
    await unmount(page);
  });

  test('each swarm links to its page and says what it is doing now', async () => {
    const page = await mount('/');
    const list = section(page, 'swarms');
    const names = Array.from(list?.querySelectorAll('[data-swarm]') ?? []).map((item) =>
      item.querySelector('a')?.getAttribute('href')
    );
    // Ждущий оператора и с недоставленным ответом -- первыми, идущий -- за ними.
    expect(names).toEqual(['/swarms/s-gate', '/swarms/s-done', '/swarms/s-busy', '/swarms/s-idle']);
    const gate = list?.querySelector('[data-swarm="s-gate"]');
    expect(gate?.textContent).toContain('gate-crew');
    expect(gate?.textContent).toContain('Waiting for answer');
    expect(gate?.textContent).toContain('Open questions: 2');
    // Акторы и шаблоны роя -- ссылками на страницы с одной его группой (#194).
    expect(gate?.querySelector('a[href="/actors?project=p1&swarm=s-gate"]')).not.toBeNull();
    expect(gate?.querySelector('a[href="/templates?project=p1&swarm=s-gate"]')).not.toBeNull();
    const done = list?.querySelector('[data-swarm="s-done"]');
    expect(done?.textContent).toContain('Succeeded');
    expect(done?.textContent).toContain('Undelivered answers: 1');
    expect(list?.querySelector('a[href="/swarms"]')).not.toBeNull();
    await unmount(page);
  });

  test('a tile of what waits for the operator leads to the swarm where to act', async () => {
    const page = await mount('/');
    const summary = section(page, 'summary');
    const waiting = summary?.querySelector('[data-tile="waiting"]');
    expect(waiting?.textContent).toContain('1');
    expect(waiting?.textContent).toContain('Open questions: 2');
    expect(waiting?.querySelector('a')?.getAttribute('href')).toBe('/swarms/s-gate');
    const undelivered = summary?.querySelector('[data-tile="undelivered"]');
    expect(undelivered?.querySelector('a')?.getAttribute('href')).toBe('/swarms/s-done');
    const running = summary?.querySelector('[data-tile="running"]');
    expect(running?.querySelector('a')?.getAttribute('href')).toBe('/swarms/s-busy');
    // Ни одного вставшего -- ссылке некуда вести.
    expect(summary?.querySelector('[data-tile="stuck"] a')).toBeNull();
    await unmount(page);
  });

  test('the page reads the project once and polls only the swarm list', async () => {
    const page = await mount('/');
    expect([...new Set(requested)].sort()).toEqual([
      '/api/projects',
      '/api/projects/p1',
      '/api/projects/p1/members',
      '/api/projects/p1/repository',
      '/api/projects/p1/swarms',
      '/api/projects/p1/workflow-catalog',
    ]);
    expect(requested.filter((url) => url === '/api/projects/p1/swarms')).toHaveLength(1);
    // Вопрос отвечен: тик перечитывает только список, и сводка следует за ним.
    swarms = [swarm('s-gate', 'gate-crew', { execution: 'running' })];
    requested = [];
    await act(async () => {
      await tickSwarmPolling();
    });
    await settle();
    expect(requested).toEqual(['/api/projects/p1/swarms']);
    expect(section(page, 'summary')?.querySelector('[data-tile="waiting"] a')).toBeNull();
    expect(section(page, 'swarms')?.querySelectorAll('[data-swarm]')).toHaveLength(1);
    await unmount(page);
  });

  test('the menu counts waiting swarms from the list the overview read', async () => {
    useSwarmActivityStore.setState({ project: '', name: '', swarms: null });
    const page = await mount('/');
    const store = useSwarmActivityStore.getState();
    expect(store.project).toBe('p1');
    expect(store.swarms?.map((item) => item.id)).toContain('s-gate');
    await unmount(page);
  });

  test('a project without swarms leads to the swarm list to create one', async () => {
    swarms = [];
    const page = await mount('/');
    const list = section(page, 'swarms');
    expect(list?.textContent).toContain('No swarms yet');
    expect(list?.querySelector('a[href="/swarms"]')).not.toBeNull();
    await unmount(page);
  });

  test('a refused catalog shows its reason and leaves the rest of the overview', async () => {
    catalog = undefined;
    const page = await mount('/');
    expect(section(page, 'workflows')?.textContent).toContain('not found');
    expect(section(page, 'swarms')?.querySelectorAll('[data-swarm]')).toHaveLength(4);
    await unmount(page);
  });

  test('unknown usage is named with its reason, not shown as zero', async () => {
    useProjectStore.setState({ project: 'p2', selectedFor: 'alice' });
    const page = await mount('/');
    const capacityText = section(page, 'capacity')?.textContent ?? '';
    expect(capacityText).toContain('inventory unavailable');
    expect(capacityText).not.toContain('0 of');
    expect(section(page, 'repository')?.textContent).toContain('Not set');
    await unmount(page);
  });

  test('the address can name the project of the overview', async () => {
    const page = await mount('/?project=p2');
    expect(useProjectStore.getState().project).toBe('p2');
    expect(page.container.querySelector('h1')?.textContent).toBe('other');
    await unmount(page);
  });
});

describe('without an active project', () => {
  test('the user chooses one of the projects open to them and stays on the overview', async () => {
    useProjectStore.setState({ project: '', selectedFor: 'alice' });
    const page = await mount('/');
    expect(text(page)).toContain('Choose a project');
    const buttons = Array.from(page.container.querySelectorAll('[data-choose-project]'));
    expect(buttons.map((button) => button.getAttribute('data-choose-project'))).toEqual([
      'p1',
      'p2',
    ]);
    await act(async () => {
      (buttons[1] as HTMLButtonElement).click();
    });
    for (let i = 0; i < 4; i += 1) await settle();
    expect(useProjectStore.getState().project).toBe('p2');
    expect(addressOf(page)).toBe('/');
    expect(page.container.querySelector('h1')?.textContent).toBe('other');
    await unmount(page);
  });

  test('a revoked project is not replaced by another one', async () => {
    useProjectStore.setState({ project: 'gone', selectedFor: 'alice' });
    const page = await mount('/');
    expect(text(page)).toContain('Choose a project');
    expect(useProjectStore.getState().project).toBe('gone');
    await unmount(page);
  });

  test('a member without projects is told to ask an administrator', async () => {
    projects = [];
    useProjectStore.setState({ project: '', selectedFor: 'alice' });
    const page = await mount('/');
    expect(text(page)).toContain('No projects yet');
    expect(text(page)).toContain('Ask an administrator');
    expect(link(page, '/projects')).toBeNull();
    await unmount(page);
  });

  test('an administrator without projects is led to create one', async () => {
    projects = [];
    useAuthStore.setState({ isSuperuser: true });
    useProjectStore.setState({ project: '', selectedFor: 'alice' });
    const page = await mount('/');
    expect(text(page)).toContain('No projects yet');
    expect(link(page, '/projects')).not.toBeNull();
    await unmount(page);
  });
});

// Таблицу маршрутов целиком bun не импортирует (страница квот сверяет классы
// CSS-модулей, а они здесь пустые): маршрут проверяется по её тексту и по
// правилу видимости, которым MainRoutes оборачивает страницы в AdminOnly.
describe('the start route', () => {
  const routes = readFileSync(new URL('../src/router/MainRoutes.tsx', import.meta.url), 'utf8');

  test('a member opens the overview at the root instead of being sent away', () => {
    expect(routes).toContain("path: '/', element: <ProjectOverviewPage />");
    expect(visibleAccessPath('/', false)).toBe(true);
  });

  test('the gateway statistics stay an administrator page at /dashboard', () => {
    expect(routes).toContain("path: '/dashboard', element: <DashboardPage />");
    expect(visibleAccessPath('/dashboard', false)).toBe(false);
    expect(visibleAccessPath('/dashboard', true)).toBe(true);
  });
});

describe('locales', () => {
  const locales = ['en', 'ru', 'zh-CN', 'zh-TW', 'vi', 'ko'];
  const read = (locale: string) =>
    JSON.parse(
      readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
    ) as Record<string, unknown>;
  const flatten = (value: unknown, prefix = ''): Record<string, string> => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) {
      return prefix ? { [prefix]: String(value ?? '') } : {};
    }
    return Object.entries(value).reduce<Record<string, string>>((leaves, [key, child]) => {
      const path = prefix ? `${prefix}.${key}` : key;
      return { ...leaves, ...flatten(child, path) };
    }, {});
  };
  const tokens = (value: string) => (value.match(/\{\{[^}]+\}\}/g) ?? []).sort().join('|');
  const english = flatten(read('en'));
  const owned = (key: string) => key.startsWith('project_overview.');

  test('every locale carries every overview key with the same interpolations', () => {
    const keys = Object.keys(english).filter(owned);
    expect(keys.length).toBeGreaterThan(20);
    for (const locale of locales) {
      const translated = flatten(read(locale));
      const ownedThere = Object.keys(translated).filter(owned).sort();
      expect({ locale, keys: ownedThere }).toEqual({ locale, keys: [...keys].sort() });
      for (const key of keys) {
        expect({ locale, key, tokens: tokens(translated[key]) }).toEqual({
          locale,
          key,
          tokens: tokens(english[key]),
        });
        expect({ locale, key, empty: translated[key].trim() === '' }).toEqual({
          locale,
          key,
          empty: false,
        });
      }
    }
  });
});
