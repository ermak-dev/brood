import { afterAll, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createMemoryRouter, MemoryRouter, RouterProvider } from 'react-router-dom';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import {
  PassComparisonContent,
  SwarmPassComparisonPage,
} from '@/features/swarms/SwarmPassComparison';
import { parsePassComparison } from '@/services/brood/passComparison';
import en from '@/i18n/locales/en.json';
import { passComparison } from './fixtures/passComparison';
import { installTestDOM } from './helpers/dom';

const restore = installTestDOM();
afterAll(restore);
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

test('comparison content labels positional alignment, unknown outcomes and both exact trace links', () => {
  const html = renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(
        MemoryRouter,
        {},
        createElement(PassComparisonContent, { comparison: parsePassComparison(passComparison()) })
      )
    )
  );
  expect(html).toContain('Visits align by recorded position, not semantic equivalence.');
  expect(html).toContain('Actual outcome comparison:');
  expect(html).toContain('Unknown');
  expect(html).toContain('/swarms/reference/passes/1');
  expect(html).toContain('/swarms/candidate/passes/10');
  expect(html).toContain(
    '/api/swarms/reference/passes/1/compare?swarm=candidate&amp;pass=10&amp;download=1'
  );
  expect(html).not.toContain('<form');
  expect(html).not.toContain('Send answer');
  expect(html).not.toContain('Run eval');
});

test('comparison visit disclosure preserves reports as escaped data, never a replay', async () => {
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        createElement(
          I18nextProvider,
          { i18n },
          createElement(
            MemoryRouter,
            {},
            createElement(PassComparisonContent, {
              comparison: parsePassComparison(passComparison()),
            })
          )
        )
      )
    );
    const details = container.querySelector('details');
    expect(details).not.toBeNull();
    await act(async () => {
      details!.open = true;
      details!.dispatchEvent(new window.Event('toggle'));
    });
    expect(details!.textContent).toContain('<script>Retained, not executed.</script>');
    expect(details!.textContent).toContain('Actual retained failure.');
    expect(details!.textContent).toContain('Reported outcome');
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('[data-action="replay"]')).toBeNull();
  } finally {
    await act(async () => root.unmount());
    container.remove();
  }
});

test('comparison requires an explicit candidate and a stale pair cannot replace the new selection', async () => {
  const original = globalThis.fetch;
  let release!: (value: Response) => void;
  let calls = 0;
  globalThis.fetch = (async (input) => {
    calls++;
    if (String(input).includes('swarm=old'))
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    return new Response(
      JSON.stringify(
        passComparison({
          candidate: { swarmId: 'current-candidate', pass: 20 },
          steps: [
            {
              ...passComparison().steps[0],
              candidate: { ...passComparison().steps[0].candidate, sequence: 20 },
            },
          ],
        })
      ),
      { status: 200 }
    );
  }) as typeof fetch;
  const router = createMemoryRouter(
    [{ path: '/swarms/:id/passes/:pass/compare', element: createElement(SwarmPassComparisonPage) }],
    { initialEntries: ['/swarms/reference/passes/1/compare'] }
  );
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        createElement(I18nextProvider, { i18n }, createElement(RouterProvider, { router }))
      )
    );
    expect(calls).toBe(0);
    await act(async () => {
      await router.navigate('/swarms/reference/passes/1/compare?swarm=old&pass=10');
    });
    expect(typeof release).toBe('function');
    await act(async () => {
      await router.navigate('/swarms/reference/passes/1/compare?swarm=current-candidate&pass=20');
    });
    expect(container.textContent).toContain('current-candidate');
    await act(async () =>
      release(
        new Response(JSON.stringify(passComparison({ candidate: { swarmId: 'old', pass: 10 } })), {
          status: 200,
        })
      )
    );
    expect(container.textContent).toContain('current-candidate');
    expect(container.querySelector('a[href="/swarms/old/passes/10"]')).toBeNull();
  } finally {
    await act(async () => root.unmount());
    router.dispose();
    container.remove();
    globalThis.fetch = original;
  }
});
