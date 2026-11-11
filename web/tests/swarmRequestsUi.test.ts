// FEATURE: Operator request inbox on the swarm page (#216, API #210).
// SPEC: Open requests on top, answered ones as history; each shows its
// question, context, who asks (actor or node) and when. A node request is
// answered by a radio group of its options (the selection is semantic, not
// a colour) with an optional comment; an actor request offers no route
// choice. After sending, a visible confirmation stays with the request and
// the history shows the chosen label. A failed delivery shows its reason and
// a retry; an unconfirmed one (#261) says the answer may have reached the
// actor and asks before a retry that may deliver it twice; after a retry
// the card shows the new state and reason. An undelivered answer stays in
// sight under its own heading and is counted in the head of the inbox until
// a delivery succeeds (#264). A refusal shows the server reason. The node request also opens at
// its node on the graph through the same card, and the page answers through
// the swarm request API instead of the old pipeline-run route. The form is
// one, in the inbox (#259): the card at the node leads to it and, once
// answered, shows the chosen label; focus after the answer goes to the
// confirmation or the refusal in the inbox, where the request keeps its place.
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
import { useSwarmRequests, type RequestInboxModel } from '@/features/swarms/useSwarmRequests';
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
  answers: Array<{ id: string; body: RequestAnswer }>;
  deliveries: string[];
  /** Запросы, к форме которых вели от узла. */
  reached: string[];
}

const recorder = (): Recorded => ({ answers: [], deliveries: [], reached: [] });

const model = (over: Partial<RequestInboxModel> = {}, recorded?: Recorded): RequestInboxModel => ({
  requests: [actorAsk(), nodeAsk(), answeredNode],
  error: '',
  refresh: async () => {},
  canAnswer: true,
  notices: {},
  handledHere: [],
  retriedHere: [],
  focus: null,
  focused: () => {},
  answer: async (request, body) => {
    recorded?.answers.push({ id: request.id, body });
  },
  reach: (request) => {
    recorded?.reached.push(request.id);
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
/** Подпись счётчика с любым числом: шаблон локали, где вместо {{count}} цифры. */
const counted = (template: string) =>
  new RegExp(
    template
      .split('{{count}}')
      .map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
      .join('\\d+')
  );

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

const settle = (ms = 0) => act(async () => new Promise((resolve) => setTimeout(resolve, ms)));
const dialog = () => document.querySelector('[role="dialog"]') as HTMLElement | null;
const buttonNamed = (scope: ParentNode | null | undefined, name: string) =>
  [...(scope?.querySelectorAll('button') ?? [])].find((button) => button.textContent === name);
/** Подтверждение в диалоге — отправка его формы, как Enter или клик в браузере. */
async function confirmDialog() {
  const form = dialog()?.querySelector('form');
  if (!form) throw new Error('no dialog form');
  await act(async () => {
    form.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  });
  await settle();
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
    const history = text.indexOf(t.history_count.replace('{{count}}', '1'));
    expect(history).toBeGreaterThan(0);
    expect(text.indexOf('Which database')).toBeLessThan(history);
    expect(text.indexOf('Approve the proposal?')).toBeLessThan(history);
    expect(text.indexOf('keep the cache layer')).toBeGreaterThan(history);
    expect(text).toContain(t.open_count.replace('{{count}}', '2'));
  });

  // #253: the history is folded into its count. The material of every
  // request folds under its label: the question is what is asked, the
  // material is read on demand, so an open request leaves room for the graph.
  test('the history folds into its count, and the material of every request folds', () => {
    const answered = actorAsk({
      id: 'req-actor-0',
      state: 'answered',
      answeredAt: '2026-11-09T10:00:00Z',
      answeredBy: 'alice',
      response: 'SQLite',
      context: 'The draft compares SQLite and Postgres.',
    });
    const html = markup(
      createElement(RequestInbox, { model: model({ requests: [actorAsk(), answered] }) })
    );
    const material = new RegExp(
      `<details(?![^>]*\\sopen)[^>]*>\\s*<summary[^>]*>${t.context}</summary>`
    );
    const fold = html.indexOf(`>${t.history_count.replace('{{count}}', '1')}</summary>`);
    expect(fold).toBeGreaterThan(0);
    const history = html.slice(html.lastIndexOf('<details', fold));
    expect(history.slice(0, history.indexOf('>'))).not.toContain(' open');
    // The open request before the history: its question in full, its material folded.
    const open = html.slice(0, html.lastIndexOf('<details', fold));
    expect(visibleText(open)).toContain('Which database should the proposal assume?');
    expect(open).toMatch(material);
    expect(open).toContain('The draft compares SQLite and Postgres.');
    // Inside the history the material is a fold of its own, under the same label.
    expect(history.slice(history.indexOf('</summary>'))).toMatch(material);
    expect(history).toContain('The draft compares SQLite and Postgres.');
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
    expect(text).not.toContain('<details');
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
    const recorded = recorder();
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
    const recorded = recorder();
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
        },
      ]);
    } finally {
      await unmount(view);
    }
  });

  // #259: one form per request. The card at the node shows that the node
  // waits and leads to the form in the inbox instead of repeating it.
  test('at the node the card leads to the inbox form instead of a second one', async () => {
    const recorded = recorder();
    const view = await mount(
      createElement(RequestCard, {
        model: model({}, recorded),
        request: nodeAsk(),
        variant: 'node',
      })
    );
    try {
      expect(view.container.textContent).not.toContain('Approve the proposal?');
      expect(view.container.textContent).toContain(t.states.open);
      expect(view.container.querySelector('form')).toBeNull();
      expect(view.container.querySelector('[role="radiogroup"]')).toBeNull();
      expect(view.container.querySelector('textarea')).toBeNull();
      const above = [...view.container.querySelectorAll('button')].find(
        (button) => button.textContent === t.answer_above
      );
      expect(above?.getAttribute('type')).toBe('button');
      await click(above);
      expect(recorded.reached).toEqual(['req-node-3']);
      expect(recorded.answers).toEqual([]);
    } finally {
      await unmount(view);
    }
  });

  test('a viewer at the node gets no way to a form it cannot use', () => {
    const html = markup(
      createElement(RequestCard, {
        model: model({ canAnswer: false }),
        request: nodeAsk(),
        variant: 'node',
      })
    );
    expect(html).not.toContain(t.answer_above);
    expect(visibleText(html)).toContain(t.read_only);
  });

  test('led from the node, focus lands on the inbox form of that request, once', async () => {
    let focused = 0;
    const requests = model({
      focus: { id: 'req-node-3', target: 'form' },
      focused: () => {
        focused += 1;
      },
    });
    // Обе карточки на странице: фокус берёт форма инбокса, не карточка узла.
    const view = await mount(
      createElement(
        'div',
        null,
        createElement(RequestCard, { key: 'inbox', model: requests, request: nodeAsk() }),
        createElement(RequestCard, {
          key: 'node',
          model: requests,
          request: nodeAsk(),
          variant: 'node',
        })
      )
    );
    try {
      expect(document.activeElement).toBe(radioLabelled(view.container, 'Approve') ?? null);
      expect(focused).toBe(1);
    } finally {
      await unmount(view);
    }
  });
});

describe('actor request form', () => {
  test('a text request has no route choice; its text is required and sent alone', async () => {
    const recorded = recorder();
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
      expect(recorded.answers).toEqual([{ id: 'req-actor-1', body: { response: 'Postgres 16' } }]);
    } finally {
      await unmount(view);
    }
  });

  test('a confirmation is a yes/no radio group sent as text', async () => {
    const recorded = recorder();
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
      expect(recorded.answers).toEqual([{ id: 'req-actor-1', body: { response: 'yes' } }]);
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

  test('the confirmation in the inbox takes focus, once', async () => {
    let focused = 0;
    const requests = model({
      notices: { [answeredNode.id]: { answered: true } },
      focus: { id: answeredNode.id, target: 'result' },
      focused: () => {
        focused += 1;
      },
    });
    const view = await mount(
      createElement(
        'div',
        null,
        createElement(RequestCard, { key: 'inbox', model: requests, request: answeredNode }),
        createElement(RequestCard, {
          key: 'node',
          model: requests,
          request: answeredNode,
          variant: 'node',
        })
      )
    );
    try {
      const statuses = view.container.querySelectorAll('[role="status"]');
      expect(statuses.length).toBe(1);
      expect(statuses[0].textContent).toBe(t.sent);
      expect(document.activeElement).toBe(statuses[0]);
      expect(focused).toBe(1);
    } finally {
      await unmount(view);
    }
  });

  // #259: the node shows what was chosen, as before; the confirmation and a
  // refusal belong to the inbox card, where the answer was given.
  test('at the node the answer shows its chosen label, not a second confirmation', () => {
    const html = markup(
      createElement(RequestCard, {
        model: model({
          notices: { [answeredNode.id]: { answered: true, refusal: 'already answered' } },
        }),
        request: answeredNode,
        variant: 'node',
      })
    );
    const text = visibleText(html);
    expect(text).toContain('Approve');
    expect(text).not.toMatch(/\bsuccess\b/);
    expect(text).toContain('keep the cache layer');
    expect(html).not.toContain('role="status"');
    expect(html).not.toContain('role="alert"');
    expect(html).not.toContain(t.answer_above);
  });

  test('a request answered here keeps its place with the confirmation, unfolded', () => {
    const html = markup(
      createElement(RequestInbox, {
        model: model({
          requests: [answeredNode, nodeAsk({ id: 'req-node-0', state: 'cancelled' })],
          notices: { [answeredNode.id]: { answered: true } },
          handledHere: [answeredNode.id],
        }),
      })
    );
    const fold = html.indexOf(`>${t.history_count.replace('{{count}}', '1')}</summary>`);
    expect(fold).toBeGreaterThan(0);
    const before = html.slice(0, html.lastIndexOf('<details', fold));
    expect(before).toMatch(new RegExp(`role="status"[^>]*>${t.sent}<`));
    expect(visibleText(before)).toContain('Approve');
    expect(visibleText(before)).toContain(t.none_open);
    expect(html).not.toContain(t.open_count.replace('{{count}}', '1'));
    expect(html.slice(fold)).not.toContain(t.sent);
  });

  // #259: a request closed elsewhere refuses the answer and leaves the open
  // list; the card it was answered in stays, and focus goes to the reason.
  test('a refusal in the inbox takes focus and keeps the card in place', async () => {
    const closed = { ...answeredNode, answeredBy: 'bob' };
    let focused = 0;
    const view = await mount(
      createElement(RequestInbox, {
        model: model({
          requests: [closed],
          notices: { [closed.id]: { refusal: 'swarm request is no longer open' } },
          handledHere: [closed.id],
          focus: { id: closed.id, target: 'result' },
          focused: () => {
            focused += 1;
          },
        }),
      })
    );
    try {
      const alert = view.container.querySelector('[role="alert"]');
      expect(alert?.textContent).toContain('swarm request is no longer open');
      expect(alert?.closest('details')).toBeNull();
      expect(document.activeElement).toBe(alert);
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
  const unconfirmed: SwarmRequest = { ...failed, delivery: 'unconfirmed' };
  const unconfirmedReason =
    'delivery is unconfirmed: the answer may have reached the actor, and a retry may deliver it twice';

  test('a failed delivery shows its state, the reason and retries without a warning', async () => {
    const recorded = recorder();
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
      expect(text).not.toContain(t.unconfirmed_note);
      await click(buttonNamed(view.container, t.retry_delivery));
      await settle();
      expect(recorded.deliveries).toEqual([failed.id]);
      expect(dialog()).toBeNull();
    } finally {
      await unmount(view);
    }
  });

  test('an unconfirmed delivery says the answer may have reached the actor, with the reason', () => {
    const html = markup(
      createElement(RequestCard, {
        model: model({ notices: { [unconfirmed.id]: { deliveryError: unconfirmedReason } } }),
        request: unconfirmed,
      })
    );
    const text = visibleText(html);
    expect(html).toMatch(
      new RegExp(`data-tone="attention"[^>]*>${t.delivery_states.unconfirmed}<`)
    );
    expect(text).toContain(t.unconfirmed_note);
    expect(text).toContain(unconfirmedReason);
    expect(html).toContain(`>${t.retry_delivery}<`);
    // Without a reason from the server (a reload) the words still say it.
    const reloaded = visibleText(
      markup(createElement(RequestCard, { model: model(), request: unconfirmed }))
    );
    expect(reloaded).toContain(t.unconfirmed_note);
    expect(reloaded).not.toContain(unconfirmedReason);
  });

  test('a retry of an unconfirmed delivery asks first: cancel sends nothing', async () => {
    const recorded = recorder();
    const view = await mount(
      createElement(RequestCard, { model: model({}, recorded), request: unconfirmed })
    );
    try {
      const retry = buttonNamed(view.container, t.retry_delivery);
      expect(retry?.getAttribute('aria-haspopup')).toBe('dialog');
      await click(retry);
      await settle();
      const asked = dialog();
      expect(asked?.textContent).toContain(t.retry_title);
      expect(asked?.textContent).toContain(t.retry_twice);
      expect(recorded.deliveries).toEqual([]);
      await click(buttonNamed(asked, en.common.cancel));
      await settle(400);
      expect(dialog()).toBeNull();
      expect(recorded.deliveries).toEqual([]);

      await click(buttonNamed(view.container, t.retry_delivery));
      await settle();
      expect(dialog()?.querySelector('button[type="submit"]')?.textContent).toBe(t.retry_delivery);
      await confirmDialog();
      await settle(400);
      expect(recorded.deliveries).toEqual([unconfirmed.id]);
      expect(dialog()).toBeNull();
    } finally {
      await unmount(view);
    }
  });

  // #264: the card stays in sight after its delivery succeeds elsewhere (a
  // poll); the reason of an earlier failed retry is not the reason of a
  // delivered answer and is not shown next to it.
  test('a delivered answer drops the reason of an earlier failed retry', () => {
    const html = markup(
      createElement(RequestCard, {
        model: model({ notices: { [failed.id]: { deliveryError: 'delivery unavailable' } } }),
        request: { ...failed, delivery: 'delivered' },
      })
    );
    expect(visibleText(html)).toContain(t.delivery_states.delivered);
    expect(visibleText(html)).not.toContain('delivery unavailable');
  });

  test('a delivered answer offers no retry', () => {
    const html = markup(
      createElement(RequestCard, { model: model(), request: { ...failed, delivery: 'delivered' } })
    );
    expect(visibleText(html)).toContain(t.delivery_states.delivered);
    expect(html).not.toContain(t.retry_delivery);
    expect(visibleText(html)).not.toContain(t.unconfirmed_note);
  });
});

// The retry goes through the page state of the requests: the reply of the
// server is the new state of the delivery and its reason.
describe('retry through the swarm request API', () => {
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
  const asked = actorAsk({
    state: 'answered',
    answeredAt: '2026-11-10T08:30:00Z',
    answeredBy: 'alice',
    response: 'Postgres',
    delivery: 'unconfirmed',
  });
  const originalFetch = globalThis.fetch;

  /** Сервер запросов роя: список и повтор доставки, вызовы записаны. */
  function serve(retried: Partial<SwarmRequest>) {
    let current = asked;
    const calls: string[] = [];
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      calls.push(`${method} ${url}`);
      if (method === 'GET' && url === '/api/swarms/swarm-7f3a/requests?all=1') {
        return Response.json({ results: [current] });
      }
      if (method === 'POST' && url === `/api/swarms/swarm-7f3a/requests/${asked.id}/deliver`) {
        current = { ...asked, ...retried, deliveryError: undefined };
        return Response.json({ ...asked, ...retried });
      }
      return Response.json({ detail: 'not found' }, { status: 404 });
    }) as typeof fetch;
    return calls;
  }

  function LiveInbox() {
    return createElement(RequestInbox, { model: useSwarmRequests(swarm.id, swarm) });
  }

  async function retryConfirmed() {
    const view = await mount(createElement(LiveInbox));
    await settle();
    // #264: the undelivered answer is in sight, not folded into the history.
    const retry = buttonNamed(view.container, t.retry_delivery);
    expect(retry?.closest('details:not([open])')).toBeNull();
    await click(retry);
    await settle();
    await confirmDialog();
    await settle(400);
    return view;
  }

  test('a retry that did not leave keeps it unconfirmed and shows the new reason', async () => {
    const reason =
      'the answer was not sent: the actor session did not receive it; an earlier delivery ' +
      'is unconfirmed: the answer may have reached the actor, and a retry may deliver it twice';
    const calls = serve({ delivery: 'unconfirmed', deliveryError: reason });
    const view = await retryConfirmed();
    try {
      expect(calls.filter((call) => call.startsWith('POST'))).toEqual([
        `POST /api/swarms/swarm-7f3a/requests/${asked.id}/deliver`,
      ]);
      const text = view.container.textContent ?? '';
      expect(text).toContain(t.delivery_states.unconfirmed);
      expect(text).toContain(reason);
      expect(buttonNamed(view.container, t.retry_delivery)).toBeDefined();
    } finally {
      await unmount(view);
      globalThis.fetch = originalFetch;
    }
  });

  test('a delivered retry shows the new state, drops the retry and keeps the focus there', async () => {
    serve({ delivery: 'delivered' });
    const view = await retryConfirmed();
    try {
      const text = view.container.textContent ?? '';
      expect(text).toContain(t.delivery_states.delivered);
      expect(text).not.toContain(t.unconfirmed_note);
      expect(buttonNamed(view.container, t.retry_delivery)).toBeUndefined();
      // The retry button is gone with its focus: the focus is on the new state.
      const focused = document.activeElement;
      expect(focused?.tagName).toBe('DD');
      expect(view.container.contains(focused)).toBe(true);
      expect(focused?.textContent).toContain(t.delivery_states.delivered);
      // #264: delivered, the card keeps its place until the page is left, and
      // the head no longer counts it.
      expect(focused?.closest('details:not([open])')).toBeNull();
      expect(text).not.toMatch(counted(t.undelivered_count));
    } finally {
      await unmount(view);
      globalThis.fetch = originalFetch;
    }
  });
});

// #264: an answer the actor did not get stays in sight until a delivery
// succeeds: under its own heading between the open requests and the folded
// history, with its retry at hand, and the head of the inbox counts it next
// to the open ones. A delivered answer is history, as before.
describe('undelivered answers', () => {
  const answered = {
    state: 'answered',
    answeredAt: '2026-11-10T08:30:00Z',
    answeredBy: 'alice',
    response: 'Postgres',
  } as const;
  const failed = actorAsk({
    ...answered,
    id: 'req-actor-failed',
    question: 'Which queue should the worker use?',
    delivery: 'failed',
  });
  const unconfirmed = actorAsk({
    ...answered,
    id: 'req-actor-unconfirmed',
    question: 'Which region hosts the database?',
    delivery: 'unconfirmed',
  });
  const delivered = actorAsk({
    ...answered,
    id: 'req-actor-delivered',
    question: 'Which cache should the proposal keep?',
    delivery: 'delivered',
  });
  const heading = new RegExp(`<h\\d[^>]*>${t.attention_title}</h\\d>`);
  const badge = (text: string) => new RegExp(`data-tone="attention"[^>]*>${text}<`);

  test('an undelivered answer stays in sight under its own heading, out of the history', () => {
    const html = markup(
      createElement(RequestInbox, {
        model: model({ requests: [nodeAsk(), failed, unconfirmed, delivered] }),
      })
    );
    const fold = html.indexOf(`>${t.history_count.replace('{{count}}', '1')}</summary>`);
    expect(fold).toBeGreaterThan(0);
    const inSight = html.slice(0, html.lastIndexOf('<details', fold));
    const title = inSight.match(heading);
    expect(title).not.toBeNull();
    const start = inSight.indexOf(title?.[0] ?? '');
    // The open request first, then the group with both undelivered answers.
    expect(inSight.indexOf('Approve the proposal?')).toBeLessThan(start);
    const group = inSight.slice(start);
    expect(visibleText(group)).toContain(t.attention_hint);
    expect(visibleText(group)).toContain('Which queue should the worker use?');
    expect(visibleText(group)).toContain('Which region hosts the database?');
    expect(group.split(`>${t.retry_delivery}<`).length - 1).toBe(2);
    // The delivered answer alone is folded into the history.
    const history = visibleText(html.slice(fold));
    expect(history).toContain('Which cache should the proposal keep?');
    expect(history).not.toContain('Which queue should the worker use?');
    expect(history).not.toContain('Which region hosts the database?');
    // The head counts the open request and the undelivered answers apart.
    expect(html).toMatch(badge(t.open_count.replace('{{count}}', '1')));
    expect(html).toMatch(badge(t.undelivered_count.replace('{{count}}', '2')));
  });

  test('an undelivered answer given here keeps its place and is counted', () => {
    const html = markup(
      createElement(RequestInbox, {
        model: model({
          requests: [failed, delivered],
          notices: { [failed.id]: { answered: true, deliveryError: 'recipient is not connected' } },
          handledHere: [failed.id],
        }),
      })
    );
    const fold = html.indexOf(`>${t.history_count.replace('{{count}}', '1')}</summary>`);
    const inSight = html.slice(0, html.lastIndexOf('<details', fold));
    expect(inSight).toMatch(new RegExp(`role="status"[^>]*>${t.sent}<`));
    expect(visibleText(inSight)).toContain('recipient is not connected');
    expect(inSight).toContain(`>${t.retry_delivery}<`);
    // Shown once, at its place: no group for it alone, but it is counted.
    expect(html).not.toMatch(heading);
    expect(html.split('Which queue should the worker use?').length - 1).toBe(1);
    expect(html).toMatch(badge(t.undelivered_count.replace('{{count}}', '1')));
  });

  test('without an undelivered answer there is no such group and no such count', () => {
    const html = markup(
      createElement(RequestInbox, { model: model({ requests: [nodeAsk(), delivered] }) })
    );
    expect(html).not.toMatch(heading);
    expect(visibleText(html)).not.toContain(t.attention_hint);
    expect(visibleText(html)).not.toMatch(counted(t.undelivered_count));
  });
});

describe('readers', () => {
  test('a viewer reads requests without a form or a retry', () => {
    const html = markup(
      createElement(RequestInbox, {
        model: model({
          canAnswer: false,
          requests: [
            nodeAsk(),
            actorAsk({ state: 'answered', delivery: 'failed', response: 'x' }),
            actorAsk({
              id: 'req-actor-2',
              state: 'answered',
              delivery: 'unconfirmed',
              response: 'y',
            }),
          ],
        }),
      })
    );
    expect(html).not.toContain('<form');
    expect(html).not.toContain(t.retry_delivery);
    expect(visibleText(html)).toContain(t.read_only);
    // #264: a viewer sees the undelivered answers too, in sight.
    expect(visibleText(html)).toContain(t.attention_title);
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

  const advanced: PipelineRun = {
    ...run,
    status: 'running',
    currentNode: 'ship',
    nodes: [
      { ...run.nodes[0], status: 'done', outcome: 'success' },
      { id: 'ship', kind: 'actor', label: 'ship', status: 'active', sequence: 4 },
    ],
  };
  const shipGraph: RunGraph = {
    svg: svg.replace(
      '</svg>',
      '<g class="node node-actor" data-node-id="ship">' +
        '<rect x="60" y="40" width="50" height="20"/></g></svg>'
    ),
    nodes: [...graph.nodes, { id: 'ship', kind: 'actor', prompt: 'Ship it' }],
  };
  const closedBy = (who: string): SwarmRequest => ({
    ...nodeAsk({ swarmId: swarm.id }),
    state: 'answered',
    answeredAt: '2026-11-10T09:00:00Z',
    answeredBy: who,
    outcome: 'success',
    optionLabel: 'Approve',
  });

  /** Сервер страницы: его состояние меняет ответ на POST. */
  interface Stand {
    run: PipelineRun;
    requests: SwarmRequest[];
    posted: Array<{ url: string; body: unknown }>;
    post: (stand: Stand) => { status: number; body: unknown };
  }

  /** Живая страница роя в happy-dom на подменённом fetch. */
  async function openPage(stand: Stand) {
    const originalFetch = globalThis.fetch;
    const documentCookie = Object.getOwnPropertyDescriptor(document, 'cookie');
    Object.defineProperty(document, 'cookie', { configurable: true, value: 'csrf=token' });
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
      if (init?.method === 'POST') {
        stand.posted.push({ url, body: JSON.parse(String(init.body)) });
        const { status, body } = stand.post(stand);
        return json(body, status);
      }
      if (url === `/api/swarms/${swarm.id}`) return json(swarm);
      if (url === '/api/projects') return json([{ id: swarm.project_id, name: 'brood' }]);
      if (url === `/api/pipeline-runs/${swarm.id}`) return json(stand.run);
      if (url === `/api/pipeline-runs/${swarm.id}/graph`) return json(shipGraph);
      if (url === `/api/swarms/${swarm.id}/requests?all=1`) {
        return json({ results: stand.requests });
      }
      return json({ detail: 'not found' }, 404);
    }) as typeof fetch;
    const { SwarmPage } = await import('@/features/swarms/SwarmPage');
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
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
    await settle();
    return {
      container,
      close: async () => {
        await act(async () => root.unmount());
        container.remove();
        globalThis.fetch = originalFetch;
        if (documentCookie) Object.defineProperty(document, 'cookie', documentCookie);
        else Reflect.deleteProperty(document, 'cookie');
      },
    };
  }
  const settle = () => act(async () => new Promise((resolve) => setTimeout(resolve, 0)));
  const nodePanel = (container: HTMLElement) =>
    [...container.querySelectorAll('h2, h3')]
      .find((heading) => heading.textContent === en.swarm_page.node)
      ?.closest('section') as HTMLElement;
  /** Выбрать вариант в единственной форме и отправить её. */
  async function answerApprove(container: HTMLElement) {
    const inbox = container.querySelector('form') as HTMLFormElement;
    await click(radioLabelled(inbox, 'Approve'));
    await click(submitButton(inbox));
    await settle();
  }

  // #259: one form on the page, in the inbox. The node leads to it; after
  // the answer the graph moves on, the confirmation in the inbox takes focus
  // and the node that asked still shows the chosen label.
  test('the open node request has one form, in the inbox, and the node leads to it', async () => {
    const stand: Stand = {
      run,
      requests: [nodeAsk({ swarmId: swarm.id })],
      posted: [],
      post: (state) => {
        state.requests = [closedBy('alice')];
        state.run = advanced;
        return { status: 200, body: state.requests[0] };
      },
    };
    const page = await openPage(stand);
    const { container } = page;
    try {
      // One request, one form: in the inbox list, not in the node panel.
      expect(container.querySelectorAll('form').length).toBe(1);
      const groups = container.querySelectorAll('[role="radiogroup"]');
      expect(groups.length).toBe(1);
      const inbox = groups[0].closest('form') as HTMLFormElement;
      expect(inbox.closest('li')).not.toBeNull();
      const panel = nodePanel(container);
      expect(panel.textContent).toContain('Approve the proposal?');
      expect(panel.querySelector('form')).toBeNull();
      const above = [...panel.querySelectorAll('button')].find(
        (button) => button.textContent === t.answer_above
      );
      expect(above).toBeDefined();
      await click(above);
      expect(document.activeElement).toBe(radioLabelled(inbox, 'Approve') ?? null);
      await answerApprove(container);
      expect(stand.posted).toEqual([
        {
          url: `/api/swarms/${swarm.id}/requests/req-node-3/answer`,
          body: { outcome: 'success' },
        },
      ]);
      expect(container.querySelector('[role="radiogroup"]')).toBeNull();
      // One confirmation, in the inbox and not folded into the history; it has focus.
      const sent = [...container.querySelectorAll('[role="status"]')].filter(
        (element) => element.textContent === t.sent
      );
      expect(sent.length).toBe(1);
      expect(document.activeElement).toBe(sent[0]);
      expect(sent[0].closest('li')).not.toBeNull();
      expect(sent[0].closest('details:not([open])')).toBeNull();
      // The graph moved on to ship; the node that asked stays selected with its label.
      expect(stand.run.currentNode).toBe('ship');
      const answeredPanel = nodePanel(container);
      expect(answeredPanel.textContent).toContain('Approve the proposal?');
      expect(answeredPanel.textContent).toContain('Approve');
      expect(answeredPanel.textContent).not.toContain(t.answer_above);
      expect(answeredPanel.textContent).not.toContain(t.sent);
    } finally {
      await page.close();
    }
  });

  // #259: another manager answered first. The refused card stays where the
  // answer was given, its reason is not folded into the history, and focus
  // goes to the reason instead of being lost with the form.
  test('a refused answer keeps its card in place with the reason in focus', async () => {
    const stand: Stand = {
      run,
      requests: [nodeAsk({ swarmId: swarm.id })],
      posted: [],
      post: (state) => {
        state.requests = [closedBy('bob')];
        return { status: 409, body: { detail: 'swarm request is no longer open' } };
      },
    };
    const page = await openPage(stand);
    const { container } = page;
    try {
      await answerApprove(container);
      expect(stand.posted.length).toBe(1);
      const alert = [...container.querySelectorAll('[role="alert"]')].find((element) =>
        element.textContent?.includes('swarm request is no longer open')
      );
      expect(alert).toBeDefined();
      expect(alert?.closest('details:not([open])')).toBeNull();
      expect(alert?.closest('li')?.textContent).toContain('bob');
      expect(document.activeElement).toBe(alert ?? null);
      expect(nodePanel(container).querySelector('[role="alert"]')).toBeNull();
    } finally {
      await page.close();
    }
  });
});
