import { afterAll, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { PassTraceContent, SwarmPassTracePage } from '@/features/swarms/SwarmPassTrace';
import { parsePassTrace } from '@/services/brood/passTrace';
import en from '@/i18n/locales/en.json';
import { passTrace, traceDelivery } from './fixtures/passTrace';
import { installTestDOM } from './helpers/dom';

const restore = installTestDOM();
afterAll(restore);
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

test('trace view distinguishes actual outcome, reported result and unavailable history without actions', () => {
  const trace = parsePassTrace(passTrace({ current: false, execution: undefined }));
  const html = renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(PassTraceContent, { trace }))
  );
  expect(html).toContain('Reported result, not actual outcome');
  expect(html).toContain('No actual outcome recorded');
  expect(html).toContain('Historical execution state was not retained');
  expect(html).toContain('Unavailable history');
  expect(html).toContain('Reported work is not applied yet.');
  expect(html).toContain('&lt;script&gt;untrusted text&lt;/script&gt;');
  expect(html).not.toContain('<script>');
  expect(html).not.toContain('<form');
  expect(html).not.toContain('Send answer');
  expect(html).not.toContain('Retry delivery');
  expect(html).not.toContain('Run eval');
  expect(html).toContain('/api/swarms/swarm/passes/1?download=1');
});

test('legacy non-RFC3339 times are not invented as calendar dates', () => {
  const value = passTrace();
  value.steps[0] = { ...value.steps[0], startedAt: '0', finishedAt: '2026-11-17' };
  const html = renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(PassTraceContent, { trace: parsePassTrace(value) })
    )
  );
  expect(html).not.toContain('dateTime="0"');
  expect(html).not.toContain('dateTime="2026-11-17"');
  expect(html.split('Time not retained').length - 1).toBeGreaterThanOrEqual(2);
});

test('trace disclosure shows the original frame and missing receipt as data, never an executable replay', async () => {
  const value = passTrace();
  value.steps[0] = { ...value.steps[0], instruction: traceDelivery() };
  const trace = parsePassTrace(value);
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        createElement(I18nextProvider, { i18n }, createElement(PassTraceContent, { trace }))
      )
    );
    const details = [...container.querySelectorAll('details')].find(
      (node) => node.querySelector('summary')?.textContent === 'Original instruction and delivery'
    );
    expect(details).toBeDefined();
    await act(async () => {
      details!.open = true;
      details!.dispatchEvent(new window.Event('toggle'));
    });
    expect(details!.textContent).toContain('<script>data</script>');
    expect(details!.textContent).toContain('No receipt retained');
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('form')).toBeNull();
    expect(container.querySelector('[data-action="replay"]')).toBeNull();
  } finally {
    await act(async () => root.unmount());
    container.remove();
  }
});

test('a stale previous swarm trace cannot overwrite the current pass page', async () => {
  const originalFetch = globalThis.fetch;
  let release!: (response: Response) => void;
  globalThis.fetch = (async (input) => {
    if (String(input).includes('/swarms/swarm/'))
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    const other = passTrace({
      swarmId: 'other',
      pass: 3,
      goal: 'Only the other swarm is current.',
      steps: [{ ...passTrace().steps[0], sequence: 3 }],
      requests: [],
      gaps: [],
    });
    return new Response(JSON.stringify(other), { status: 200 });
  }) as typeof fetch;
  const router = createMemoryRouter(
    [{ path: '/swarms/:id/passes/:pass', element: createElement(SwarmPassTracePage) }],
    { initialEntries: ['/swarms/swarm/passes/1'] }
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
    expect(typeof release).toBe('function');
    await act(async () => {
      await router.navigate('/swarms/other/passes/3');
    });
    expect(container.textContent).toContain('Only the other swarm is current.');
    await act(async () => release(new Response(JSON.stringify(passTrace()), { status: 200 })));
    expect(container.textContent).toContain('Only the other swarm is current.');
    expect(container.textContent).not.toContain('Keep <script>untrusted text</script> intact.');
  } finally {
    await act(async () => root.unmount());
    container.remove();
    router.dispose();
    globalThis.fetch = originalFetch;
  }
});
