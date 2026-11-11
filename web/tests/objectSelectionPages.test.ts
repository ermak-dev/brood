// FEATURE: Pages follow the addressed selection (#194, epic #190).
// SPEC: /actors?project=P&swarm=S&actor=A selects project P, shows only the
// group of swarm S and opens actor A's panel; templates, workers and pools
// open their panels by ?template=, ?worker=, ?pool= and ?proxmox=. A row opens
// the panel by a new history entry and closing goes back to the list as it
// was; closing a panel opened by a link stays on the page without the
// parameter. An address that names what the page cannot show says so.
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
import { useProjectStore } from '@/stores';
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

const swarm = (id: string, name: string): AccessSwarm => ({
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
});

const managed = (name: string) => ({
  metadata: { name, uid: `uid-${name}` },
  provider: 'proxmox',
  phase: 'running',
  desired: 'running',
  role: 'worker',
});

const API: Record<string, unknown> = {
  '/api/projects': [
    { id: 'p0', name: 'other' },
    { id: 'p1', name: 'brood' },
  ],
  '/api/projects/p1/swarms': [swarm('s1', 'docs-crew'), swarm('s2', 'build-crew')],
  '/api/swarms/s1/actors': { results: [managed('writer')] },
  '/api/swarms/s1/templates': { results: [] },
  '/api/swarms/s2/actors': { results: [managed('builder')] },
  '/api/swarms/s2/templates': { results: [{ metadata: { name: 'golden-base', uid: 't-1' } }] },
  '/api/workers': {
    results: [{ metadata: { name: 'worker-a', uid: 'w-1' }, workerPool: 'gvisor-pool' }],
  },
  '/api/worker-pools': {
    items: [
      {
        metadata: { name: 'gvisor-pool', uid: 'pool-1' },
        spec: { replicas: 1, sandboxClasses: [] },
      },
    ],
  },
  '/api/proxmox-pools': { items: [] },
};

let requested: string[] = [];
const originalFetch = globalThis.fetch;
beforeEach(() => {
  requested = [];
  globalThis.fetch = (async (input: string | URL | Request) => {
    const url = String(input);
    requested.push(url);
    const body = API[url];
    return body === undefined
      ? new Response(JSON.stringify({ detail: 'not found' }), { status: 404 })
      : new Response(JSON.stringify(body), { status: 200 });
  }) as typeof fetch;
  useProjectStore.setState({ project: 'p0', selectedFor: 'alice' });
});
afterEach(() => {
  globalThis.fetch = originalFetch;
});

function Address() {
  const { pathname, search } = useLocation();
  return createElement('output', { 'data-location': `${pathname}${search}` });
}
const addressOf = (mounted: Mounted) =>
  mounted.container.querySelector('output')?.getAttribute('data-location');

const settle = () => act(async () => new Promise((resolve) => setTimeout(resolve, 0)));
// Лист уезжает 280 мс и только потом сообщает о закрытии.
const slideAway = () => act(async () => new Promise((resolve) => setTimeout(resolve, 320)));

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
    createElement(Route, {
      key: 'p',
      path: '/worker-pools',
      element: createElement(WorkerPoolsPage),
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

const groups = (mounted: Mounted) =>
  [...mounted.container.querySelectorAll('h2')].map((node) => node.textContent);
const dialog = () => document.body.querySelector('[role="dialog"]');
const dialogTitle = () => dialog()?.querySelector('h2, h3')?.textContent ?? null;
const notice = (mounted: Mounted) =>
  mounted.container.querySelector('[data-selection-notice]')?.textContent ?? null;

async function click(node: Element | null | undefined) {
  expect(node).toBeTruthy();
  await act(async () => {
    (node as HTMLElement).dispatchEvent(new MouseEvent('click', { bubbles: true }));
  });
  for (let i = 0; i < 3; i += 1) await settle();
}

const closeButton = () => dialog()?.querySelector(`button[aria-label="${en.common.close}"]`);
const row = (mounted: Mounted, name: string) =>
  [...mounted.container.querySelectorAll('tr[tabindex="0"]')].find((tr) =>
    tr.textContent?.includes(name)
  );

describe('an actor link', () => {
  test('selects the project, shows its swarm only and opens the actor', async () => {
    const page = await mount('/actors?project=p1&swarm=s2&actor=builder');
    try {
      expect(useProjectStore.getState().project).toBe('p1');
      expect(groups(page)).toEqual(['build-crew']);
      expect(requested).toContain('/api/swarms/s2/actors');
      expect(requested).not.toContain('/api/swarms/s1/actors');
      expect(dialogTitle()).toContain('builder');
    } finally {
      await unmount(page);
    }
  });

  test('closing its panel keeps the page and drops only the actor', async () => {
    const page = await mount('/actors?project=p1&swarm=s2&actor=builder&probe=1');
    try {
      await click(closeButton());
      await slideAway();
      expect(addressOf(page)).toBe('/actors?project=p1&swarm=s2&probe=1');
      expect(dialog()).toBeNull();
      expect(groups(page)).toEqual(['build-crew']);
    } finally {
      await unmount(page);
    }
  });

  test('an actor that is not in the swarm is named, not replaced', async () => {
    const page = await mount('/actors?project=p1&swarm=s2&actor=ghost');
    try {
      expect(dialog()).toBeNull();
      expect(notice(page)).toContain('ghost');
      await click(page.container.querySelector('[data-selection-notice] button'));
      expect(addressOf(page)).toBe('/actors?project=p1&swarm=s2');
      expect(notice(page)).toBeNull();
    } finally {
      await unmount(page);
    }
  });

  test('a name without a swarm opens nothing: names repeat across swarms', async () => {
    const page = await mount('/actors?project=p1&actor=builder');
    try {
      expect(groups(page)).toEqual(['docs-crew', 'build-crew']);
      expect(dialog()).toBeNull();
    } finally {
      await unmount(page);
    }
  });
});

describe('a row', () => {
  test('opens the panel by its address and closing returns to the list as it was', async () => {
    const page = await mount('/actors?project=p1');
    try {
      expect(groups(page)).toEqual(['docs-crew', 'build-crew']);
      await click(row(page, 'builder'));
      expect(addressOf(page)).toBe('/actors?project=p1&swarm=s2&actor=builder');
      expect(dialogTitle()).toContain('builder');
      await click(closeButton());
      await slideAway();
      for (let i = 0; i < 3; i += 1) await settle();
      expect(addressOf(page)).toBe('/actors?project=p1');
      expect(groups(page)).toEqual(['docs-crew', 'build-crew']);
      expect(dialog()).toBeNull();
    } finally {
      await unmount(page);
    }
  });
});

describe('the swarm filter', () => {
  test('a swarm that is not active in the project is named, all swarms are one click away', async () => {
    const page = await mount('/actors?project=p1&swarm=gone');
    try {
      expect(groups(page)).toEqual([]);
      expect(notice(page)).toContain(en.access.swarm_unavailable);
      await click(page.container.querySelector('[data-selection-notice] button'));
      expect(addressOf(page)).toBe('/actors?project=p1');
      expect(groups(page)).toEqual(['docs-crew', 'build-crew']);
    } finally {
      await unmount(page);
    }
  });
});

describe('other object pages', () => {
  test('a template link opens the template in its swarm', async () => {
    const page = await mount('/templates?project=p1&swarm=s2&template=golden-base');
    try {
      expect(useProjectStore.getState().project).toBe('p1');
      expect(groups(page)).toEqual(['build-crew']);
      expect(dialogTitle()).toBe('golden-base');
    } finally {
      await unmount(page);
    }
  });

  test('a worker link opens the worker', async () => {
    const page = await mount('/workers?worker=worker-a');
    try {
      expect(dialogTitle()).toBe('worker-a');
      await click(closeButton());
      await slideAway();
      expect(addressOf(page)).toBe('/workers');
    } finally {
      await unmount(page);
    }
  });

  test('a pool link opens the pool; an unknown one is named', async () => {
    const page = await mount('/worker-pools?pool=gvisor-pool');
    try {
      expect(dialogTitle()).toBe('gvisor-pool');
    } finally {
      await unmount(page);
    }
    const missing = await mount('/worker-pools?pool=nope');
    try {
      expect(dialog()).toBeNull();
      expect(notice(missing)).toContain('nope');
    } finally {
      await unmount(missing);
    }
  });
});
