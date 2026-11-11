// FEATURE: Panels name related objects by links (#195, epic #190).
// SPEC: The actor panel names its swarm (by name, not ID), its template and,
// in its placement, its worker and pool; the template panel its swarm, the
// actors made from it and the pools they run on; the worker panel its pool;
// the pool panel its workers; the swarm page links its actors, templates and
// access with their counts, read once and not by the page's tick. A worker
// or pool is a link for an administrator only (those pages are admin-only),
// an actor only when its panel would open. A link leads to the panel.
import { afterAll, afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { act, createElement, type ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import { ActorsPage } from '@/features/substrate/ActorsPage';
import { TemplatesPage } from '@/features/substrate/TemplatesPage';
import { WorkersPage } from '@/features/substrate/WorkersPage';
import { WorkerPoolsPage } from '@/features/substrate/WorkerPoolsPage';
import { PolicyPage } from '@/features/substrate/PolicyPage';
import { SwarmPage } from '@/features/swarms/SwarmPage';
import { tickSwarmPolling } from '@/features/swarms/useSwarmPolling';
import { useAuthStore, useProjectStore } from '@/stores';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
// Хранилище сессии (администратор) пишет в localStorage: на время файла -- память.
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

const swarm = (role: AccessSwarm['role'] = 'owner'): AccessSwarm => ({
  id: 's1',
  project_id: 'p1',
  owner_id: 'u1',
  owner_username: 'alice',
  role,
  name: 'docs-crew',
  atespace_name: 'space-s1',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-01T09:00:00Z',
});

const luna = {
  metadata: { name: 'luna', uid: 'uid-luna' },
  actorTemplate: { atespace: 'space-s1', name: 'claude' },
  status: {
    state: 'ACTOR_STATE_RUNNING',
    workerAssignment: {
      worker: { name: 'worker-7' },
      workerNamespace: 'ate-system',
      workerPool: 'gvisor-pool',
      workerPod: 'gvisor-pool-5d8f-abc',
      nodeName: 'node-a',
    },
  },
};
const coder = {
  metadata: { name: 'coder', uid: 'uid-coder' },
  provider: 'proxmox',
  actorTemplate: { atespace: 'space-s1', name: 'claude-proxmox-pve' },
  placement: { node: 'pve-a', ctid: 105, pool: 'pve' },
  phase: 'running',
  desired: 'running',
  role: 'worker',
  generation: '3',
};
const templates = [
  { metadata: { name: 'claude', uid: 't-claude' } },
  { metadata: { name: 'claude-proxmox-pve', uid: 't-pve' }, provider: 'proxmox', readOnly: true },
];

let role: AccessSwarm['role'] = 'owner';
const API = (): Record<string, unknown> => ({
  '/api/projects': [{ id: 'p1', name: 'brood' }],
  '/api/projects/p1/swarms': [swarm(role)],
  '/api/swarms/s1': swarm(role),
  '/api/swarms/s1/actors': { results: [luna, coder] },
  '/api/swarms/s1/templates': { results: templates },
  '/api/swarms/s1/members': [
    { user_id: 'u1', username: 'alice', display_name: '', role: 'owner', disabled: false },
    { user_id: 'u2', username: 'bob', display_name: '', role: 'viewer', disabled: false },
  ],
  '/api/swarms/s1/requests?all=1': { results: [] },
  '/api/swarms/s1/roles': { swarmId: 's1', execution: 'idle', graph: null, roles: [] },
  '/api/workers': {
    results: [
      { metadata: { name: 'worker-8', uid: 'w-8' }, workerPool: 'gvisor-pool' },
      { metadata: { name: 'worker-7', uid: 'w-7' }, workerPool: 'gvisor-pool' },
      { metadata: { name: 'worker-9', uid: 'w-9' }, workerPool: 'kata-pool' },
    ],
  },
  '/api/worker-pools': {
    items: [
      { metadata: { name: 'gvisor-pool', uid: 'pool-1' }, spec: { replicas: 2, sandboxClasses: [] } },
    ],
  },
  '/api/proxmox-pools': { items: [] },
  '/api/identities': [],
  '/api/policy/global': { metadata: { name: 'global', uid: 'pol-1', version: '1' }, bindings: [] },
});

let requested: string[] = [];
const originalFetch = globalThis.fetch;
beforeEach(() => {
  requested = [];
  role = 'owner';
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
  const routes: ReactNode[] = [
    createElement(Route, { key: 'a', path: '/actors', element: createElement(ActorsPage) }),
    createElement(Route, { key: 't', path: '/templates', element: createElement(TemplatesPage) }),
    createElement(Route, { key: 'w', path: '/workers', element: createElement(WorkersPage) }),
    createElement(Route, { key: 'p', path: '/worker-pools', element: createElement(WorkerPoolsPage) }),
    createElement(Route, { key: 's', path: '/swarms/:id', element: createElement(SwarmPage) }),
    createElement(Route, {
      key: 'policy',
      path: '/substrate-policy',
      element: createElement(PolicyPage),
    }),
  ];
  await act(async () =>
    root.render(
      createElement(
        I18nextProvider,
        { i18n },
        createElement(
          MemoryRouter,
          { initialEntries: [entry] },
          createElement(Routes, null, routes),
          createElement(Address)
        )
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
const dialog = () => document.body.querySelector('[role="dialog"]');
/** Подпись над заголовком листа -- рой объекта. */
const eyebrow = () => dialog()?.querySelector('h2')?.previousElementSibling?.textContent ?? null;
/** Ссылки внутри узла: адрес и видимый текст. */
const links = (node: Element | null | undefined) =>
  Array.from(node?.querySelectorAll('a') ?? []).map((a) => [
    a.getAttribute('href'),
    a.textContent,
  ]);
/** Значение строки сведений по подписи. */
const fact = (node: Element | null | undefined, label: string) =>
  Array.from(node?.querySelectorAll('dt') ?? []).find((dt) => dt.textContent === label)
    ?.nextElementSibling;
const relations = () => dialog()?.querySelector('[data-relations]');

async function click(node: Element | null | undefined) {
  expect(node).toBeTruthy();
  await act(async () => {
    (node as HTMLElement).dispatchEvent(new MouseEvent('click', { bubbles: true, button: 0 }));
  });
  for (let i = 0; i < 4; i += 1) await settle();
}

describe('the actor panel', () => {
  test('names its swarm by name and links its swarm and template', async () => {
    const page = await mount('/actors?project=p1&swarm=s1&actor=luna');
    try {
      expect(eyebrow()).toBe('docs-crew');
      expect(links(relations())).toEqual([
        ['/swarms/s1', 'docs-crew'],
        ['/templates?project=p1&swarm=s1&template=claude', 'claude'],
      ]);
    } finally {
      await unmount(page);
    }
  });

  test('a native actor is placed on the worker and pool of its assignment', async () => {
    const member = await mount('/actors?project=p1&swarm=s1&actor=luna');
    try {
      expect(fact(dialog(), en.actors.worker)?.textContent).toBe('worker-7');
      expect(fact(dialog(), en.relations.worker_pool)?.textContent).toBe('gvisor-pool');
      // Workers and pools are admin pages: a member gets their names, no links.
      expect(links(fact(dialog(), en.actors.worker))).toEqual([]);
    } finally {
      await unmount(member);
    }
    useAuthStore.setState({ isSuperuser: true });
    const admin = await mount('/actors?project=p1&swarm=s1&actor=luna');
    try {
      expect(links(fact(dialog(), en.actors.worker))).toEqual([
        ['/workers?worker=worker-7', 'worker-7'],
      ]);
      expect(links(fact(dialog(), en.relations.worker_pool))).toEqual([
        ['/worker-pools?pool=gvisor-pool', 'gvisor-pool'],
      ]);
    } finally {
      await unmount(admin);
    }
  });

  test('a managed actor links its virtual template and, for an administrator, its pool', async () => {
    useAuthStore.setState({ isSuperuser: true });
    const page = await mount('/actors?project=p1&swarm=s1&actor=coder');
    try {
      expect(eyebrow()).toBe('docs-crew');
      expect(links(relations())).toEqual([
        ['/swarms/s1', 'docs-crew'],
        ['/templates?project=p1&swarm=s1&template=claude-proxmox-pve', 'claude-proxmox-pve'],
      ]);
      expect(links(fact(dialog(), en.actors.pool))).toEqual([
        ['/worker-pools?proxmox=pve', 'pve'],
      ]);
    } finally {
      await unmount(page);
    }
  });

  test('the template link opens the template panel in the same swarm', async () => {
    const page = await mount('/actors?project=p1&swarm=s1&actor=luna');
    try {
      await click(relations()?.querySelector('a[href^="/templates"]'));
      expect(addressOf(page)).toBe('/templates?project=p1&swarm=s1&template=claude');
      expect(dialog()?.querySelector('h2')?.textContent).toBe('claude');
    } finally {
      await unmount(page);
    }
  });
});

describe('the template panel', () => {
  test('names its swarm and lists the actors made from it', async () => {
    const page = await mount('/templates?project=p1&swarm=s1&template=claude');
    try {
      expect(eyebrow()).toBe('docs-crew');
      expect(links(fact(relations(), en.relations.swarm))).toEqual([['/swarms/s1', 'docs-crew']]);
      expect(links(fact(relations(), en.relations.actors))).toEqual([
        ['/actors?project=p1&swarm=s1&actor=luna', 'luna'],
      ]);
      expect(fact(relations(), en.relations.actor_pools)?.textContent).toBe('gvisor-pool');
    } finally {
      await unmount(page);
    }
  });

  test('a viewer gets the native actor by name: its panel would not open', async () => {
    role = 'viewer';
    const page = await mount('/templates?project=p1&swarm=s1&template=claude');
    try {
      expect(fact(relations(), en.relations.actors)?.textContent).toBe('luna');
      expect(links(fact(relations(), en.relations.actors))).toEqual([]);
    } finally {
      await unmount(page);
    }
    const managed = await mount('/templates?project=p1&swarm=s1&template=claude-proxmox-pve');
    try {
      expect(links(fact(relations(), en.relations.actors))).toEqual([
        ['/actors?project=p1&swarm=s1&actor=coder', 'coder'],
      ]);
    } finally {
      await unmount(managed);
    }
  });
});

describe('worker and pool', () => {
  test('the worker panel links its pool', async () => {
    useAuthStore.setState({ isSuperuser: true });
    const page = await mount('/workers?worker=worker-7');
    try {
      expect(links(fact(dialog(), en.workers.pool))).toEqual([
        ['/worker-pools?pool=gvisor-pool', 'gvisor-pool'],
      ]);
    } finally {
      await unmount(page);
    }
  });

  test('the pool panel lists its workers; the page without a panel reads none', async () => {
    useAuthStore.setState({ isSuperuser: true });
    const list = await mount('/worker-pools');
    try {
      expect(requested).not.toContain('/api/workers');
    } finally {
      await unmount(list);
    }
    const page = await mount('/worker-pools?pool=gvisor-pool');
    try {
      expect(links(dialog()).filter(([href]) => href?.startsWith('/workers'))).toEqual([
        ['/workers?worker=worker-7', 'worker-7'],
        ['/workers?worker=worker-8', 'worker-8'],
      ]);
    } finally {
      await unmount(page);
    }
  });
});

describe('the swarm page', () => {
  const objects = (mounted: Mounted) =>
    mounted.container.querySelector('[data-swarm-objects]');

  test('links the swarm actors, templates and access with their counts', async () => {
    const page = await mount('/swarms/s1');
    try {
      expect(links(objects(page))).toEqual([
        ['/actors?project=p1&swarm=s1', `${en.swarm_page.objects.actors}2`],
        ['/templates?project=p1&swarm=s1', `${en.swarm_page.objects.templates}2`],
      ]);
      expect(objects(page)?.querySelector('button')?.textContent).toBe(
        `${en.swarm_page.objects.access}2`
      );
      await click(objects(page)?.querySelector('button'));
      expect(dialog()?.querySelector('h2')?.textContent).toBe(en.access.swarm_access);
    } finally {
      await unmount(page);
    }
  });

  test('the counts are read once on opening, not by the page tick', async () => {
    const page = await mount('/swarms/s1');
    try {
      const reads = (url: string) => requested.filter((item) => item === url).length;
      const lists = () => ({
        // Актора читает и блок ролей: управляющий выбирает из них.
        actors: reads('/api/swarms/s1/actors'),
        templates: reads('/api/swarms/s1/templates'),
        members: reads('/api/swarms/s1/members'),
      });
      const opened = lists();
      expect(opened.templates).toBe(1);
      expect(opened.members).toBe(1);
      expect(opened.actors).toBeGreaterThan(0);
      await act(async () => {
        await tickSwarmPolling();
      });
      for (let i = 0; i < 3; i += 1) await settle();
      expect(lists()).toEqual(opened);
    } finally {
      await unmount(page);
    }
  });

  test('a list that cannot be read is a link without a number, not zero', async () => {
    const base = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request) =>
      String(input) === '/api/swarms/s1/templates'
        ? new Response(JSON.stringify({ detail: 'forbidden' }), { status: 403 })
        : base(input)) as typeof fetch;
    const page = await mount('/swarms/s1');
    try {
      expect(links(objects(page))[1]).toEqual([
        '/templates?project=p1&swarm=s1',
        en.swarm_page.objects.templates,
      ]);
    } finally {
      await unmount(page);
    }
  });
});

describe('the policy page', () => {
  test('names the chosen swarm, not its ID', async () => {
    const page = await mount('/substrate-policy');
    try {
      await click(page.container.querySelector('[aria-haspopup="listbox"]'));
      await click(
        Array.from(document.querySelectorAll('[role="option"]')).find(
          (option) => option.textContent === 'docs-crew'
        )
      );
      const scopes = Array.from(
        page.container.querySelectorAll(`[aria-label="${en.policy.scope}"] button`)
      ).map((button) => button.textContent);
      expect(scopes).toEqual([en.policy.scope_global, 'Swarm docs-crew']);
      expect(page.container.textContent).not.toContain('Swarm s1');
    } finally {
      await unmount(page);
    }
  });
});
