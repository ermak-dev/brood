// FEATURE: Operator request inbox on the swarm page (#216, API #210).
// SPEC: Open requests on top, answered ones as history; each shows its
// question, context, who asks (actor or node) and when. A node request is
// answered by a radio group of its options (the selection is semantic, not
// a colour) with an optional comment; an actor request offers no route
// choice. After sending, a visible confirmation stays with the request and
// the history shows the chosen label. A failed delivery shows its reason and
// a retry; a refusal shows the server reason. The node request also opens at
// its node on the graph through the same card, and the page answers through
// the swarm request API instead of the old pipeline-run route.
import { afterAll, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { act, createElement, type ReactNode } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun, RunGraph } from '@/services/brood/pipelines';
import type { RequestAnswer, SwarmRequest } from '@/services/brood/requests';
import { RequestCard, RequestInbox } from '@/features/swarms/SwarmRequests';
import type { RequestInboxModel } from '@/features/swarms/useSwarmRequests';
import { SwarmView } from '@/features/swarms/SwarmPage';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
afterAll(restoreDOM);
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
const t = en.swarm_requests;

const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');

const nodeAsk = (over: Partial<SwarmRequest> = {}): SwarmRequest => ({
  id: 'req-node-3',
  swarmId: 'swarm-7f3a',
  origin: 'node',
  sequence: 3,
  nodeId: 'decision',
  kind: 'choice',
  question: 'Approve the proposal?',
  context: '',
  options: [
    { label: 'Approve', outcome: 'success' },
    { label: 'Reject', outcome: 'fail' },
  ],
  state: 'open',
  createdAt: '2026-11-10T08:40:06Z',
  ...over,
});

const actorAsk = (over: Partial<SwarmRequest> = {}): SwarmRequest => ({
  id: 'req-actor-1',
  swarmId: 'swarm-7f3a',
  origin: 'actor',
  actor: { name: 'architect', incarnation: 2 },
  kind: 'text',
  question: 'Which database should the proposal assume?',
  context: 'The draft compares SQLite and Postgres.',
  options: [],
  state: 'open',
  createdAt: '2026-11-10T08:20:00Z',
  ...over,
});

const answeredNode = nodeAsk({
  id: 'req-node-1',
  sequence: 1,
  state: 'answered',
  answeredAt: '2026-11-09T11:02:00Z',
  answeredBy: 'alice',
  outcome: 'success',
  optionLabel: 'Approve',
  response: 'keep the cache layer',
});

interface Recorded {
  answers: Array<{ id: string; body: RequestAnswer; place: string }>;
  deliveries: string[];
}

const model = (over: Partial<RequestInboxModel> = {}, recorded?: Recorded): RequestInboxModel => ({
  requests: [actorAsk(), nodeAsk(), answeredNode],
  error: '',
  refresh: async () => {},
  canAnswer: true,
  notices: {},
  focus: null,
  focused: () => {},
  answer: async (request, body, place) => {
    recorded?.answers.push({ id: request.id, body, place });
  },
  deliver: async (request) => {
    recorded?.deliveries.push(request.id);
  },
  ...over,
});

const wrap = (node: ReactNode) =>
  createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null, node));
const markup = (node: ReactNode) => renderToStaticMarkup(wrap(node));
const visibleText = (html: string) => html.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ');

/** Живой рендер в happy-dom: клики и отправка формы, как в браузере. */
async function mount(node: ReactNode): Promise<{ container: HTMLElement; root: Root }> {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  await act(async () => root.render(wrap(node)));
  return { container, root };
}

async function unmount({ container, root }: { container: HTMLElement; root: Root }) {
  await act(async () => root.unmount());
  container.remove();
}

const radios = (container: HTMLElement) =>
  [...container.querySelectorAll('input[type="radio"]')] as HTMLInputElement[];
const radioLabelled = (container: HTMLElement, label: string) =>
  radios(container).find((input) => input.closest('label')?.textContent?.includes(label));
const submitButton = (container: HTMLElement) =>
  container.querySelector('button[type="submit"]') as HTMLButtonElement;
const form = (container: HTMLElement) => container.querySelector('form') as HTMLFormElement;
const click = (element: Element | null | undefined) =>
  act(async () => (element as HTMLElement).click());

/** Ввод текста. Поддержку события input React определяет при загрузке
 * react-dom, а bun загружает его раньше, чем тест ставит DOM: событие input
 * до onChange не доходит. Поэтому поле получает значение, а обработчик
 * React — то же событие, что дал бы браузер. */
async function type(field: HTMLTextAreaElement, value: string) {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(
      Object.getPrototypeOf(field) as object,
      'value'
    )?.set;
    setter?.call(field, value);
    const props = Object.entries(field).find(([key]) => key.startsWith('__reactProps$'))?.[1] as
      { onChange?: (event: { target: HTMLTextAreaElement }) => void } | undefined;
    props?.onChange?.({ target: field });
  });
}

describe('inbox', () => {
  test('open requests come first; answered ones follow as history', () => {
    const html = markup(createElement(RequestInbox, { model: model() }));
    const text = visibleText(html);
    const history = text.indexOf(t.history);
    expect(history).toBeGreaterThan(0);
    expect(text.indexOf('Which database')).toBeLessThan(history);
    expect(text.indexOf('Approve the proposal?')).toBeLessThan(history);
    expect(text.indexOf('keep the cache layer')).toBeGreaterThan(history);
    expect(text).toContain(t.open_count.replace('{{count}}', '2'));
  });

  test('each request shows its question, context, who asks, the node and when', () => {
    const html = markup(createElement(RequestInbox, { model: model(), onSelectNode: () => {} }));
    const text = visibleText(html);
    expect(text).toContain('The draft compares SQLite and Postgres.');
    expect(text).toContain(t.context);
    expect(text).toContain(t.asker_actor);
    expect(text).toContain('architect');
    expect(text).toContain(t.asker_node);
    expect(html).toMatch(/<button[^>]*>decision<\/button>/);
    expect(html).toContain('dateTime="2026-11-10T08:40:06Z"');
    expect(html).toContain('dateTime="2026-11-10T08:20:00Z"');
  });

  test('an actor that is gone is named as such, not as an empty name', () => {
    const html = markup(
      createElement(RequestInbox, {
        model: model({ requests: [actorAsk({ actor: { name: '', incarnation: 2 } })] }),
      })
    );
    expect(visibleText(html)).toContain(t.actor_gone);
  });

  test('without requests the inbox says there is nothing to answer', () => {
    const text = visibleText(
      markup(createElement(RequestInbox, { model: model({ requests: [] }) }))
    );
    expect(text).toContain(t.none_open);
    expect(text).not.toContain(t.history);
  });

  test('a failed read shows its reason and a retry', () => {
    const html = markup(
      createElement(RequestInbox, { model: model({ requests: null, error: 'internal error' }) })
    );
    expect(html).toContain('role="alert"');
    expect(html).toContain('internal error');
  });
});

describe('node request form', () => {
  test('the options are a radio group by label; nothing is chosen and sent at first', async () => {
    const recorded: Recorded = { answers: [], deliveries: [] };
    const view = await mount(
      createElement(RequestCard, { model: model({}, recorded), request: nodeAsk() })
    );
    try {
      const group = view.container.querySelector('[role="radiogroup"]');
      expect(group?.getAttribute('aria-labelledby')).toBeTruthy();
      expect(radios(view.container).map((input) => input.closest('label')?.textContent)).toEqual([
        'Approve',
        'Reject',
      ]);
      expect(radios(view.container).some((input) => input.checked)).toBe(false);
      expect(submitButton(view.container).disabled).toBe(true);
      // A forced submit without a choice still sends nothing.
      await act(async () => {
        // Событие окна happy-dom: глобальный Event у bun свой, DOM его не принимает.
        form(view.container).dispatchEvent(
          new window.Event('submit', { bubbles: true, cancelable: true })
        );
      });
      expect(recorded.answers).toEqual([]);
    } finally {
      await unmount(view);
    }
  });

  test('the chosen option is checked, not only coloured, and its outcome is sent', async () => {
    const recorded: Recorded = { answers: [], deliveries: [] };
    const view = await mount(
      createElement(RequestCard, { model: model({}, recorded), request: nodeAsk() })
    );
    try {
      await click(radioLabelled(view.container, 'Approve'));
      expect(radioLabelled(view.container, 'Approve')?.checked).toBe(true);
      expect(radioLabelled(view.container, 'Reject')?.checked).toBe(false);
      await click(radioLabelled(view.container, 'Reject'));
      expect(radioLabelled(view.container, 'Approve')?.checked).toBe(false);
      expect(radioLabelled(view.container, 'Reject')?.checked).toBe(true);
      const comment = view.container.querySelector('textarea') as HTMLTextAreaElement;
      expect(comment.required).toBe(false);
      await type(comment, 'needs a second architect');
      expect(submitButton(view.container).disabled).toBe(false);
      await click(submitButton(view.container));
      expect(recorded.answers).toEqual([
        {
          id: 'req-node-3',
          body: { outcome: 'fail', response: 'needs a second architect' },
          place: 'inbox',
        },
      ]);
    } finally {
      await unmount(view);
    }
  });

  test('at the node the same card answers without repeating the node question', async () => {
    const recorded: Recorded = { answers: [], deliveries: [] };
    const view = await mount(
      createElement(RequestCard, {
        model: model({}, recorded),
        request: nodeAsk(),
        variant: 'node',
      })
    );
    try {
      expect(view.container.textContent).not.toContain('Approve the proposal?');
      expect(view.container.querySelector('[role="radiogroup"]')?.getAttribute('aria-label')).toBe(
        'Approve the proposal?'
      );
      await click(radioLabelled(view.container, 'Approve'));
      await click(submitButton(view.container));
      expect(recorded.answers).toEqual([
        { id: 'req-node-3', body: { outcome: 'success' }, place: 'node' },
      ]);
    } finally {
      await unmount(view);
    }
  });
});

describe('actor request form', () => {
  test('a text request has no route choice; its text is required and sent alone', async () => {
    const recorded: Recorded = { answers: [], deliveries: [] };
    const view = await mount(
      createElement(RequestCard, { model: model({}, recorded), request: actorAsk() })
    );
    try {
      expect(radios(view.container)).toEqual([]);
      const field = view.container.querySelector('textarea') as HTMLTextAreaElement;
      expect(field.required).toBe(true);
      expect(submitButton(view.container).disabled).toBe(true);
      await type(field, 'Postgres 16');
      await click(submitButton(view.container));
      expect(recorded.answers).toEqual([
        { id: 'req-actor-1', body: { response: 'Postgres 16' }, place: 'inbox' },
      ]);
    } finally {
      await unmount(view);
    }
  });

  test('a confirmation is a yes/no radio group sent as text', async () => {
    const recorded: Recorded = { answers: [], deliveries: [] };
    const view = await mount(
      createElement(RequestCard, {
        model: model({}, recorded),
        request: actorAsk({ kind: 'confirm' }),
      })
    );
    try {
      expect(radios(view.container).map((input) => input.closest('label')?.textContent)).toEqual([
        t.confirm.yes,
        t.confirm.no,
      ]);
      expect(view.container.querySelector('textarea')).toBeNull();
      await click(radioLabelled(view.container, t.confirm.yes));
      await click(submitButton(view.container));
      expect(recorded.answers).toEqual([
        { id: 'req-actor-1', body: { response: 'yes' }, place: 'inbox' },
      ]);
    } finally {
      await unmount(view);
    }
  });

  // #256: подсказка под формой описывала поле только зрительно.
  test('the answer field is described by the hint of its request kind', () => {
    const html = markup(createElement(RequestCard, { model: model(), request: actorAsk() }));
    const field = html.match(/<textarea[^>]*>/)?.[0] ?? '';
    const describedBy = field.match(/aria-describedby="([^"]+)"/)?.[1];
    expect(describedBy).toBeTruthy();
    expect(html).toMatch(new RegExp(`id="${describedBy}"[^>]*>${t.actor_hint.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}<`));
  });

  test('the actor hint says the answer does not route the graph', () => {
    const text = visibleText(
      markup(createElement(RequestCard, { model: model(), request: actorAsk() }))
    );
    expect(text).toContain(t.actor_hint);
    expect(text).not.toContain(t.route_hint);
  });
});

describe('after the answer', () => {
  test('a visible confirmation stays with the request and the label is shown', () => {
    const html = markup(
      createElement(RequestCard, {
        model: model({ notices: { [answeredNode.id]: { answered: true } } }),
        request: answeredNode,
      })
    );
    expect(html).toMatch(new RegExp(`role="status"[^>]*>${t.sent}<`));
    const text = visibleText(html);
    expect(text).toContain('Approve');
    expect(text).not.toMatch(/\bsuccess\b/);
    expect(text).toContain('keep the cache layer');
    expect(text).toContain('alice');
    expect(html).not.toContain('type="radio"');
  });

  test('the confirmation takes focus where the answer was sent, once', async () => {
    let focused = 0;
    const view = await mount(
      createElement(RequestCard, {
        model: model({
          notices: { [answeredNode.id]: { answered: true } },
          focus: { id: answeredNode.id, place: 'inbox' },
          focused: () => {
            focused += 1;
          },
        }),
        request: answeredNode,
      })
    );
    try {
      const status = view.container.querySelector('[role="status"]');
      expect(status?.textContent).toBe(t.sent);
      expect(document.activeElement).toBe(status);
      expect(focused).toBe(1);
    } finally {
      await unmount(view);
    }
  });

  test('a refusal shows the server reason', () => {
    const html = markup(
      createElement(RequestCard, {
        model: model({
          notices: { [answeredNode.id]: { refusal: 'swarm request is no longer open' } },
        }),
        request: answeredNode,
      })
    );
    expect(html).toMatch(/role="alert"[^>]*>[^<]*swarm request is no longer open/);
  });

  test('a cancelled request says it closed without an answer', () => {
    const text = visibleText(
      markup(
        createElement(RequestCard, { model: model(), request: nodeAsk({ state: 'cancelled' }) })
      )
    );
    expect(text).toContain(t.states.cancelled);
    expect(text).toContain(t.cancelled_hint);
  });
});

describe('delivery to the actor', () => {
  const failed = actorAsk({
    state: 'answered',
    answeredAt: '2026-11-10T08:30:00Z',
    answeredBy: 'alice',
    response: 'Postgres',
    delivery: 'failed',
  });

  test('a failed delivery shows its state, the reason and a retry', async () => {
    const recorded: Recorded = { answers: [], deliveries: [] };
    const view = await mount(
      createElement(RequestCard, {
        model: model(
          { notices: { [failed.id]: { deliveryError: 'recipient is not connected' } } },
          recorded
        ),
        request: failed,
      })
    );
    try {
      const text = view.container.textContent ?? '';
      expect(text).toContain(t.delivery_states.failed);
      expect(text).toContain('recipient is not connected');
      const retry = [...view.container.querySelectorAll('button')].find(
        (button) => button.textContent === t.retry_delivery
      );
      await click(retry);
      expect(recorded.deliveries).toEqual([failed.id]);
    } finally {
      await unmount(view);
    }
  });

  test('a delivered answer offers no retry', () => {
    const html = markup(
      createElement(RequestCard, { model: model(), request: { ...failed, delivery: 'delivered' } })
    );
    expect(visibleText(html)).toContain(t.delivery_states.delivered);
    expect(html).not.toContain(t.retry_delivery);
  });
});

describe('readers', () => {
  test('a viewer reads requests without a form or a retry', () => {
    const html = markup(
      createElement(RequestInbox, {
        model: model({
          canAnswer: false,
          requests: [nodeAsk(), actorAsk({ state: 'answered', delivery: 'failed', response: 'x' })],
        }),
      })
    );
    expect(html).not.toContain('<form');
    expect(html).not.toContain(t.retry_delivery);
    expect(visibleText(html)).toContain(t.read_only);
  });
});

describe('swarm page', () => {
  const swarm: AccessSwarm = {
    id: 'swarm-7f3a',
    project_id: 'project-4b8c',
    owner_id: 'user-9a8b',
    owner_username: 'alice',
    role: 'owner',
    name: 'release-crew',
    atespace_name: 'brood-7f3a',
    state: 'active',
    created_at: '2026-11-01T09:00:00Z',
    updated_at: '2026-11-02T09:00:00Z',
  };
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 80">
<g class="node node-human state-active" data-node-id="decision"><rect x="0" y="40" width="50" height="20"/><text x="25" y="54">decision</text></g>
</svg>`;
  const graph: RunGraph = {
    svg,
    nodes: [{ id: 'decision', kind: 'human', question: 'Approve the proposal?' }],
  };
  const run: PipelineRun = {
    id: swarm.id,
    name: 'swarm-architecture',
    status: 'waiting_human',
    swarmId: swarm.id,
    startedAt: null,
    currentNode: 'decision',
    nodes: [{ id: 'decision', kind: 'human', label: 'decision', status: 'active', sequence: 3 }],
  };

  test('the inbox sits on the page before the graph, even for a swarm without a graph', () => {
    const view = (over: Partial<Parameters<typeof SwarmView>[0]>) =>
      markup(
        createElement(SwarmView, {
          swarm,
          projectName: 'brood',
          run,
          runLoading: false,
          runError: '',
          retryRun: async () => {},
          graph,
          graphLoading: false,
          graphError: '',
          retryGraph: async () => {},
          selected: 'decision',
          onSelect: () => {},
          requests: createElement('section', { 'data-inbox': 'yes' }),
          ...over,
        })
      );
    const html = view({});
    expect(html.indexOf('data-inbox')).toBeGreaterThan(0);
    expect(html.indexOf('data-inbox')).toBeLessThan(html.indexOf(en.swarm_page.graph));
    expect(view({ run: null, graph: null, selected: undefined })).toContain('data-inbox');
  });

  test('the page answers through the swarm request API, not the pipeline-run route', () => {
    const page = source('features/swarms/SwarmPage.tsx');
    expect(page).not.toContain('answerGate');
    expect(page).not.toContain('HumanGatePanel');
    expect(page).toContain('useSwarmRequests');
  });

  test('the open node request is answered once from the inbox and shows at its node', async () => {
    let requests: SwarmRequest[] = [nodeAsk({ swarmId: swarm.id })];
    const posted: Array<{ url: string; body: unknown }> = [];
    const originalFetch = globalThis.fetch;
    const documentCookie = Object.getOwnPropertyDescriptor(document, 'cookie');
    Object.defineProperty(document, 'cookie', { configurable: true, value: 'csrf=token' });
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
      if (init?.method === 'POST') {
        posted.push({ url, body: JSON.parse(String(init.body)) });
        const answered = {
          ...requests[0],
          state: 'answered',
          answeredAt: '2026-11-10T09:00:00Z',
          answeredBy: 'alice',
          outcome: 'success',
          optionLabel: 'Approve',
        };
        requests = [answered];
        return json(answered);
      }
      if (url === `/api/swarms/${swarm.id}`) return json(swarm);
      if (url === '/api/projects') return json([{ id: swarm.project_id, name: 'brood' }]);
      if (url === `/api/pipeline-runs/${swarm.id}`) return json(run);
      if (url === `/api/pipeline-runs/${swarm.id}/graph`) return json(graph);
      if (url === `/api/swarms/${swarm.id}/requests?all=1`) return json({ results: requests });
      return json({ detail: 'not found' }, 404);
    }) as typeof fetch;
    const { SwarmPage } = await import('@/features/swarms/SwarmPage');
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    try {
      await act(async () =>
        root.render(
          createElement(
            I18nextProvider,
            { i18n },
            createElement(
              MemoryRouter,
              { initialEntries: [`/swarms/${swarm.id}`] },
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
      await act(async () => new Promise((resolve) => setTimeout(resolve, 0)));
      // The inbox card and the card at the selected node: two forms, one request.
      const groups = container.querySelectorAll('[role="radiogroup"]');
      expect(groups.length).toBe(2);
      const inbox = groups[0].closest('form') as HTMLFormElement;
      const approve = [...inbox.querySelectorAll('input[type="radio"]')].find((input) =>
        input.closest('label')?.textContent?.includes('Approve')
      );
      await click(approve);
      await click(inbox.querySelector('button[type="submit"]'));
      await act(async () => new Promise((resolve) => setTimeout(resolve, 0)));
      expect(posted).toEqual([
        {
          url: `/api/swarms/${swarm.id}/requests/req-node-3/answer`,
          body: { outcome: 'success' },
        },
      ]);
      expect(container.querySelector('[role="radiogroup"]')).toBeNull();
      // Confirmation at the inbox card and at the node card; focus at the inbox one.
      const sent = [...container.querySelectorAll('[role="status"]')].filter(
        (element) => element.textContent === t.sent
      );
      expect(sent.length).toBe(2);
      expect(document.activeElement).toBe(sent[0]);
      expect(sent[0].closest('li')).not.toBeNull();
      expect(container.textContent).toContain('Approve');
    } finally {
      await act(async () => root.unmount());
      container.remove();
      globalThis.fetch = originalFetch;
      if (documentCookie) Object.defineProperty(document, 'cookie', documentCookie);
      else Reflect.deleteProperty(document, 'cookie');
    }
  });
});
