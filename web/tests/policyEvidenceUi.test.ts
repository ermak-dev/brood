import { afterAll, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { RequestCard } from '@/features/swarms/SwarmRequests';
import type { RequestInboxModel } from '@/features/swarms/useSwarmRequests';
import type { SwarmRequest } from '@/services/brood/requests';
import en from '@/i18n/locales/en.json';
import { installTestDOM } from './helpers/dom';
import { candidate } from './fixtures/policyEvidence';

const restore = installTestDOM();
afterAll(restore);
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

const model: RequestInboxModel = {
  requests: [],
  error: '',
  refresh: async () => {},
  canAnswer: false,
  notices: {},
  handledHere: [],
  retriedHere: [],
  focus: null,
  focused: () => {},
  answer: async () => {
    throw new Error('read-only inspection cannot answer');
  },
  deliver: async () => {
    throw new Error('policy has no delivery');
  },
  reach: () => {},
};
const request: SwarmRequest = {
  id: 'policy:candidate',
  swarmId: 'swarm',
  origin: 'policy',
  revision: 1,
  kind: 'confirm',
  question: 'Approve automatic transition?',
  context: '',
  options: [],
  state: 'open',
  createdAt: '2026-11-16T08:00:00Z',
};

test('evidence from a previous swarm cannot overwrite the current disclosure', async () => {
  const originalFetch = globalThis.fetch;
  let release!: (response: Response) => void;
  globalThis.fetch = (async (input) => {
    if (String(input).includes('/swarms/swarm/'))
      return new Promise<Response>((resolve) => {
        release = resolve;
      });
    return new Response(JSON.stringify(candidate({ id: 'next', swarmId: 'other' })), {
      status: 200,
    });
  }) as typeof fetch;
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  const render = (item: SwarmRequest) =>
    createElement(I18nextProvider, { i18n }, createElement(RequestCard, { request: item, model }));
  const open = async () =>
    act(async () => {
      const details = container.querySelector('details')!;
      details.open = true;
      details.dispatchEvent(new window.Event('toggle'));
    });
  try {
    await act(async () => root.render(render(request)));
    await open();
    expect(typeof release).toBe('function');
    await act(async () => root.render(render({ ...request, swarmId: 'other', id: 'policy:next' })));
    expect(container.querySelector('pre')).toBeNull();
    await open();
    await act(async () => release(new Response(JSON.stringify(candidate()), { status: 200 })));
    expect(JSON.parse(container.querySelector('pre')!.textContent!).swarmId).toBe('other');
    expect(JSON.parse(container.querySelector('pre')!.textContent!).id).toBe('next');
  } finally {
    await act(async () => root.unmount());
    container.remove();
    globalThis.fetch = originalFetch;
  }
});

test('a viewer can load complete immutable policy evidence without answering or executing paths', async () => {
  const originalFetch = globalThis.fetch;
  const calls: Array<{ url: string; method: string }> = [];
  globalThis.fetch = (async (input, init) => {
    calls.push({ url: String(input), method: init?.method ?? 'GET' });
    return new Response(JSON.stringify(candidate()), { status: 200 });
  }) as typeof fetch;
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        createElement(I18nextProvider, { i18n }, createElement(RequestCard, { request, model }))
      )
    );
    expect(calls).toEqual([]);
    const details = [...container.querySelectorAll('details')].find(
      (x) => x.querySelector('summary')?.textContent === 'Policy decision evidence'
    );
    expect(details).toBeDefined();
    await act(async () => {
      details!.open = true;
      details!.dispatchEvent(new window.Event('toggle'));
    });
    expect(calls).toEqual([
      { url: '/api/swarms/swarm/requests/policy%3Acandidate/policy', method: 'GET' },
    ]);
    const snapshot = container.querySelector('pre');
    expect(snapshot).not.toBeNull();
    expect(JSON.parse(snapshot!.textContent!)).toEqual(candidate());
    expect(container.querySelector('script')).toBeNull();
    expect(container.textContent).toContain(
      'Retained decision, not a current permission to proceed. This snapshot is shown without truncation.'
    );
    expect(container.querySelector('form')).toBeNull();
    expect(container.querySelectorAll('input[type=radio]')).toHaveLength(0);
  } finally {
    await act(async () => root.unmount());
    container.remove();
    globalThis.fetch = originalFetch;
  }
});

test('a viewer can inspect a retained terminal result without a fake exit or retry', async () => {
  const originalFetch = globalThis.fetch;
  const terminal = candidate({ terminal: true, target: '' });
  const calls: string[] = [];
  globalThis.fetch = (async (_input, init) => {
    calls.push(init?.method ?? 'GET');
    return new Response(JSON.stringify(terminal), { status: 200 });
  }) as typeof fetch;
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        createElement(
          I18nextProvider,
          { i18n },
          createElement(RequestCard, {
            request: { ...request, question: 'Finalize the reported result without a successor?' },
            model,
          })
        )
      )
    );
    await act(async () => {
      const details = container.querySelector('details')!;
      details.open = true;
      details.dispatchEvent(new window.Event('toggle'));
    });
    const snapshot = container.querySelector('pre');
    expect(snapshot).not.toBeNull();
    expect(JSON.parse(snapshot!.textContent!)).toEqual(terminal);
    expect(container.textContent).toContain('Finalize the reported result without a successor?');
    expect(calls).toEqual(['GET']);
    expect(container.querySelector('form')).toBeNull();
    expect(container.querySelectorAll('input[type=radio]')).toHaveLength(0);
  } finally {
    await act(async () => root.unmount());
    container.remove();
    globalThis.fetch = originalFetch;
  }
});
