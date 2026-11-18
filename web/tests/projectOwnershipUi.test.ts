// FEATURE: Явный owner проекта редактирует policy, но не назначает владельцев.
// SPEC: Unknown/error ownership не открывает права, admin сохраняет свои права.
import { afterAll, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter } from 'react-router-dom';
import {
  ProjectOwnershipPanel,
  ProjectPolicyManagement,
} from '@/features/access/ProjectPolicyManagement';
import { useAuthStore } from '@/stores';
import type { ProjectOwnership } from '@/services/brood/projectOwnership';
import en from '@/i18n/locales/en.json';
import { installTestDOM } from './helpers/dom';

const restore = installTestDOM();
const storageBefore = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
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
  restore();
  if (storageBefore) Object.defineProperty(globalThis, 'localStorage', storageBefore);
  else Reflect.deleteProperty(globalThis, 'localStorage');
});
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
const wrap = (node: ReturnType<typeof createElement>) =>
  createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null, node));
const ownership: ProjectOwnership = {
  projectId: 'p',
  viewerUid: 'alice-id',
  revision: 1,
  ownerUid: 'alice-id',
  ownerUsername: 'alice',
  updatedBy: 'admin',
  updatedAt: 'now',
};
const base = {
  ownership,
  loading: false,
  error: '',
  administrator: false,
  busy: false,
  submit: (_username: string) => {},
  retry: async () => {},
};

test('only administrator gets an ownership assignment form', () => {
  const owner = renderToStaticMarkup(wrap(createElement(ProjectOwnershipPanel, base)));
  expect(owner).toContain('alice');
  expect(owner).not.toContain('<form');
  const admin = renderToStaticMarkup(
    wrap(createElement(ProjectOwnershipPanel, { ...base, administrator: true }))
  );
  expect(admin).toContain('<form');
  expect(admin).toContain('name="username"');
  expect(admin).toContain('value="alice"');
});

test('unknown or errored ownership never exposes an assignment form', () => {
  for (const state of [
    { ownership: undefined, loading: true, error: '' },
    { ownership, loading: false, error: 'Ownership unavailable' },
  ]) {
    const html = renderToStaticMarkup(
      wrap(
        createElement(ProjectOwnershipPanel, {
          ...base,
          ...state,
          administrator: true,
        })
      )
    );
    expect(html).not.toContain('<form');
  }
});

test('owner labels remain escaped rather than executable markup', () => {
  const html = renderToStaticMarkup(
    wrap(
      createElement(ProjectOwnershipPanel, {
        ...base,
        ownership: { ...ownership, ownerUsername: '<script>unsafe()</script>' },
      })
    )
  );
  expect(html).not.toContain('<script>');
  expect(html).toContain('&lt;script&gt;unsafe()&lt;/script&gt;');
});

for (const scenario of ['owner', 'member', 'unknown', 'administrator'] as const) {
  test(`policy edit action follows current ownership: ${scenario}`, async () => {
    const originalFetch = globalThis.fetch;
    const previousAuth = useAuthStore.getState();
    useAuthStore.setState({ isSuperuser: scenario === 'administrator' });
    globalThis.fetch = (async (url) => {
      if (String(url).endsWith('/owner')) {
        if (scenario === 'unknown' || scenario === 'administrator') {
          return new Response(JSON.stringify({ detail: 'Ownership unavailable' }), { status: 503 });
        }
        return new Response(
          JSON.stringify({
            ...ownership,
            ownerUid: scenario === 'owner' ? 'alice-id' : 'other-id',
          }),
          { status: 200 }
        );
      }
      return new Response(
        JSON.stringify({
          projectId: 'p',
          revision: 0,
          rules: [],
          createdBy: '',
          createdAt: '',
        }),
        { status: 200 }
      );
    }) as typeof fetch;
    const container = document.createElement('div');
    document.body.append(container);
    const root = createRoot(container);
    try {
      await act(async () =>
        root.render(wrap(createElement(ProjectPolicyManagement, { projectId: 'p' })))
      );
      const edits = [...container.querySelectorAll('button')].filter(
        (button) => button.textContent === en.workflow_policy.edit
      );
      expect(edits.length).toBe(scenario === 'owner' || scenario === 'administrator' ? 1 : 0);
      expect(container.querySelector('form')).toBeNull();
    } finally {
      await act(async () => root.unmount());
      container.remove();
      globalThis.fetch = originalFetch;
      useAuthStore.setState(previousAuth);
    }
  });
}
