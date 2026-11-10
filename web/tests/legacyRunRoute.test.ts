// FEATURE: Old run addresses lead to the swarm (#217, epic #204).
// SPEC: The Workflows section is gone and a run is its swarm (#206): the old
// run addresses /workflows/runs/:id and /pipelines/:id open the swarm page of
// the same ID, /workflows and /pipelines open the swarm list. The query string
// goes along (?project= still picks the project), and members reach the
// redirect instead of being sent to the project list.
import { afterAll, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { act, createElement, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import { visibleAccessPath } from '@/features/access/permissions';
import { LegacyRunRoute } from '@/features/swarms/LegacyRunRoute';
import { SwarmPage } from '@/features/swarms/SwarmPage';
import { legacyRunPath } from '@/features/swarms/routes';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

// MainRoutes целиком в bun test не импортируется (страницы квот требуют
// настоящих CSS-модулей): пути берутся из таблицы маршрутов, а переходят
// по ним настоящие переадресация и страница роя.
const routes = readFileSync(new URL('../src/router/MainRoutes.tsx', import.meta.url), 'utf8');
const servedBy = (element: string) =>
  [...routes.matchAll(new RegExp(`path: '([^']+)', element: <${element} />`, 'g'))].map(
    (match) => match[1]
  );
const legacyPaths = servedBy('LegacyRunRoute');

const OLD_ADDRESSES = ['/workflows', '/workflows/runs/:id', '/pipelines', '/pipelines/:id'];

describe('route table', () => {
  test('every old Workflows and Pipelines address redirects, none serves a page', () => {
    const old = [
      ...routes.matchAll(/path: '(\/(?:workflows|pipelines)(?:\/[^']*)?)', element: <(\w+)/g),
    ].map(([, path, element]) => [path, element]);
    expect(old).toEqual(OLD_ADDRESSES.map((path) => [path, 'LegacyRunRoute']));
    expect(legacyPaths).toEqual(OLD_ADDRESSES);
    expect(servedBy('SwarmPage')).toEqual(['/swarms/:id']);
  });

  test('a member following an old link reaches the redirect', () => {
    for (const path of [...legacyPaths, '/workflows/runs/run-1', '/pipelines/run-1']) {
      expect([path, visibleAccessPath(path, false)]).toEqual([path, true]);
    }
  });
});

describe('redirect target', () => {
  test('a run becomes the swarm of the same, escaped ID', () => {
    expect(legacyRunPath('swarm-1', '')).toBe('/swarms/swarm-1');
    expect(legacyRunPath('a/b c', '')).toBe('/swarms/a%2Fb%20c');
    expect(legacyRunPath('x?y', '')).toBe('/swarms/x%3Fy');
  });

  test('the section becomes the swarm list; the query string goes along', () => {
    expect(legacyRunPath(undefined, '')).toBe('/swarms');
    expect(legacyRunPath('', '')).toBe('/swarms');
    expect(legacyRunPath(undefined, '?project=p')).toBe('/swarms?project=p');
    expect(legacyRunPath('swarm-1', '?project=p')).toBe('/swarms/swarm-1?project=p');
  });
});

const swarm: AccessSwarm = {
  id: 's/2',
  project_id: 'project-4b8c1d2e',
  owner_id: 'user-9a8b7c6d',
  owner_username: 'alice',
  role: 'owner',
  name: 'second-crew',
  atespace_name: 'brood-7f3a9c2e',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-02T09:00:00Z',
};

function Location() {
  const { pathname, search } = useLocation();
  return createElement('output', { 'data-location': `${pathname}${search}` });
}

/** Открывает адрес в роутере с переадресациями и страницей роя из таблицы
 * маршрутов; возвращает, где оказался роутер и что запросила страница. */
async function follow(entry: string) {
  const requested: string[] = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (url: string) => {
    requested.push(url);
    const body =
      url === `/api/swarms/${encodeURIComponent(swarm.id)}`
        ? swarm
        : url === '/api/projects'
          ? [{ id: swarm.project_id, name: 'brood' }]
          : { detail: 'not found' };
    return new Response(JSON.stringify(body), { status: 'detail' in body ? 404 : 200 });
  }) as typeof fetch;
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  const table: ReactNode[] = [
    ...legacyPaths.map((path) =>
      createElement(Route, { key: path, path, element: createElement(LegacyRunRoute) })
    ),
    createElement(Route, { key: 'list', path: '/swarms', element: 'swarm list' }),
    createElement(Route, { key: 'swarm', path: '/swarms/:id', element: createElement(SwarmPage) }),
  ];
  try {
    await act(async () =>
      root.render(
        createElement(
          I18nextProvider,
          { i18n },
          createElement(
            MemoryRouter,
            { initialEntries: [entry] },
            createElement(Routes, null, table),
            createElement(Location)
          )
        )
      )
    );
    await act(async () => new Promise((resolve) => setTimeout(resolve, 0)));
    return {
      location: container.querySelector('output')?.getAttribute('data-location'),
      title: container.querySelector('h1')?.textContent,
      text: container.textContent ?? '',
      requested,
    };
  } finally {
    await act(async () => root.unmount());
    container.remove();
    globalThis.fetch = originalFetch;
  }
}

describe('following an old address', () => {
  test('an old run address opens the page of its swarm', async () => {
    const id = encodeURIComponent(swarm.id);
    for (const entry of [`/workflows/runs/${id}`, `/pipelines/${id}`]) {
      const page = await follow(`${entry}?project=p`);
      expect({ entry, location: page.location }).toEqual({
        entry,
        location: `/swarms/${id}?project=p`,
      });
      expect(page.requested).toContain(`/api/swarms/${id}`);
      expect(page.title).toBe('second-crew');
    }
  });

  test('the old section address opens the swarm list with its query', async () => {
    for (const entry of ['/workflows', '/pipelines', '/workflows?project=p']) {
      const page = await follow(entry);
      expect({ entry, location: page.location }).toEqual({
        entry,
        location: `/swarms${entry.includes('?') ? '?project=p' : ''}`,
      });
      expect(page.text).toContain('swarm list');
    }
  });
});
