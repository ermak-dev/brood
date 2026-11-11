// FEATURE: Operator requests of a swarm on its page (#216, API #210).
// SPEC: A node request routes the graph only through one of its options,
// with an optional comment; an actor request never routes: its choice or
// confirmation is the answer text. Open requests come first, answered ones
// stay as history with the label that was chosen; a request answered on the
// page keeps its place among the open ones (#259). Only managers of an
// active swarm answer; a failed or unconfirmed delivery to an actor can be
// retried, and only the unconfirmed one may deliver the answer twice (#261).
// Every key of the request inbox exists in all six locales.
import { afterEach, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import type { AccessSwarm } from '@/services/brood/access';
import { DELIVERY_STATES, requestsApi, type SwarmRequest } from '@/services/brood/requests';
import {
  answerBody,
  answerSummary,
  canAnswerRequests,
  deliveryRetryable,
  emptyDraft,
  inboxSections,
  nodeRequest,
  requestForm,
  retryMayDuplicate,
} from '@/features/swarms/requestState';

const nodeAsk = (over: Partial<SwarmRequest> = {}): SwarmRequest => ({
  id: 'req-node-3',
  swarmId: 'swarm-1',
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
  swarmId: 'swarm-1',
  origin: 'actor',
  actor: { name: 'architect', incarnation: 2 },
  kind: 'text',
  question: 'Which database should the proposal assume?',
  context: 'The current draft compares SQLite and Postgres.',
  options: [],
  state: 'open',
  createdAt: '2026-11-10T08:20:00Z',
  ...over,
});

const swarm = (over: Partial<AccessSwarm> = {}): AccessSwarm => ({
  id: 'swarm-1',
  project_id: 'project-1',
  owner_id: 'user-1',
  owner_username: 'alice',
  role: 'owner',
  name: 'release-crew',
  atespace_name: 'brood-1',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-02T09:00:00Z',
  ...over,
});

const draft = (choice: string | null, text = '') => ({ ...emptyDraft(), choice, text });

describe('node request answer', () => {
  test('a node request is never sent without a chosen option', () => {
    expect(answerBody(nodeAsk(), emptyDraft())).toBeNull();
    expect(answerBody(nodeAsk(), draft(null, 'approve with a caveat'))).toBeNull();
  });

  test('the outcome is always one of the request options', () => {
    expect(answerBody(nodeAsk(), draft('ship-it'))).toBeNull();
    // The label is not an outcome: the route takes the outcome, not the label.
    expect(answerBody(nodeAsk(), draft('Approve'))).toBeNull();
    expect(answerBody(nodeAsk(), draft('success'))).toEqual({ outcome: 'success' });
    expect(answerBody(nodeAsk(), draft('fail'))).toEqual({ outcome: 'fail' });
  });

  test('the comment is optional and travels with the outcome', () => {
    expect(answerBody(nodeAsk(), draft('success', '  keep the cache layer  '))).toEqual({
      outcome: 'success',
      response: 'keep the cache layer',
    });
    expect(answerBody(nodeAsk(), draft('success', '   '))).toEqual({ outcome: 'success' });
  });

  test('the form offers the options by label and routes by outcome', () => {
    expect(requestForm(nodeAsk())).toEqual({
      choices: [
        { value: 'success', label: 'Approve' },
        { value: 'fail', label: 'Reject' },
      ],
      route: true,
      confirm: false,
      text: 'optional',
    });
  });
});

describe('actor request answer', () => {
  test('an actor request never carries a route outcome, whatever its kind', () => {
    const asks = [
      [actorAsk(), draft(null, 'Postgres')],
      [actorAsk({ kind: 'confirm' }), draft('yes')],
      [
        actorAsk({
          kind: 'choice',
          options: [
            { label: 'SQLite', outcome: 'SQLite' },
            { label: 'Postgres', outcome: 'Postgres' },
          ],
        }),
        draft('Postgres'),
      ],
    ] as const;
    for (const [ask, value] of asks) {
      const body = answerBody(ask, value);
      expect(body).not.toBeNull();
      expect(body).not.toHaveProperty('outcome');
      expect(requestForm(ask)?.route).toBe(false);
    }
  });

  test('a text request needs text and offers no choice at all', () => {
    expect(requestForm(actorAsk())).toEqual({
      choices: [],
      route: false,
      confirm: false,
      text: 'required',
    });
    expect(answerBody(actorAsk(), draft(null, '   '))).toBeNull();
    expect(answerBody(actorAsk(), draft(null, ' Postgres 16 \n'))).toEqual({
      response: 'Postgres 16',
    });
    // A stray choice does not turn the text request into a routed one.
    expect(answerBody(actorAsk(), draft('success', 'Postgres'))).toEqual({ response: 'Postgres' });
  });

  test('a confirmation answers yes or no as its text', () => {
    const ask = actorAsk({ kind: 'confirm' });
    expect(requestForm(ask)).toMatchObject({ confirm: true, route: false, text: 'none' });
    expect(requestForm(ask)?.choices.map((choice) => choice.value)).toEqual(['yes', 'no']);
    expect(answerBody(ask, emptyDraft())).toBeNull();
    expect(answerBody(ask, draft('maybe'))).toBeNull();
    expect(answerBody(ask, draft('no'))).toEqual({ response: 'no' });
  });

  test('a choice of an actor answers with the chosen label as its text', () => {
    const ask = actorAsk({
      kind: 'choice',
      options: [
        { label: 'SQLite', outcome: 'SQLite' },
        { label: 'Postgres', outcome: 'Postgres' },
      ],
    });
    expect(requestForm(ask)).toMatchObject({ route: false, text: 'none' });
    expect(answerBody(ask, draft('MySQL'))).toBeNull();
    expect(answerBody(ask, draft('SQLite'))).toEqual({ response: 'SQLite' });
  });

  test('a request of an unknown origin offers no form instead of guessing', () => {
    expect(requestForm(actorAsk({ origin: 'robot' }))).toBeNull();
    expect(answerBody(actorAsk({ origin: 'robot' }), draft(null, 'hi'))).toBeNull();
  });
});

describe('inbox order', () => {
  test('open requests come first, oldest first; history follows, newest first', () => {
    const requests = [
      nodeAsk({ id: 'answered-old', state: 'answered', answeredAt: '2026-11-09T10:00:00Z' }),
      nodeAsk({ id: 'open-new', createdAt: '2026-11-10T09:00:00Z' }),
      actorAsk({ id: 'cancelled', state: 'cancelled', createdAt: '2026-11-10T07:00:00Z' }),
      actorAsk({ id: 'open-old', createdAt: '2026-11-10T08:00:00Z' }),
      actorAsk({ id: 'answered-new', state: 'answered', answeredAt: '2026-11-10T08:30:00Z' }),
    ];
    const { open, current, history } = inboxSections(requests);
    expect(open.map((request) => request.id)).toEqual(['open-old', 'open-new']);
    expect(current).toEqual(open);
    expect(history.map((request) => request.id)).toEqual([
      'answered-new',
      'cancelled',
      'answered-old',
    ]);
    expect(inboxSections(null)).toEqual({ open: [], current: [], history: [] });
  });

  // #259: the inbox is the one place to answer. A request answered on this
  // page keeps its place among the open ones, so its confirmation is not
  // folded away into the history; the open count still counts only the open.
  test('a request answered on this page keeps its place, out of the history', () => {
    const requests = [
      nodeAsk({ id: 'open-new', createdAt: '2026-11-10T09:00:00Z' }),
      nodeAsk({
        id: 'answered-here',
        state: 'answered',
        createdAt: '2026-11-10T08:30:00Z',
        answeredAt: '2026-11-10T09:10:00Z',
      }),
      actorAsk({ id: 'open-old', createdAt: '2026-11-10T08:00:00Z' }),
      nodeAsk({ id: 'answered-old', state: 'answered', answeredAt: '2026-11-09T10:00:00Z' }),
    ];
    const ids = (list: SwarmRequest[]) => list.map((request) => request.id);
    const sections = inboxSections(requests, ['answered-here']);
    expect(ids(sections.open)).toEqual(['open-old', 'open-new']);
    expect(ids(sections.current)).toEqual(['open-old', 'answered-here', 'open-new']);
    expect(ids(sections.history)).toEqual(['answered-old']);
  });
});

describe('stored answer', () => {
  test('a node answer shows the chosen label, not the machine outcome', () => {
    const answered = nodeAsk({
      state: 'answered',
      answeredAt: '2026-11-10T09:00:00Z',
      answeredBy: 'alice',
      outcome: 'success',
      optionLabel: 'Approve',
      response: 'with the cache layer',
    });
    expect(answerSummary(answered)).toEqual({ choice: 'Approve', text: 'with the cache layer' });
    // Without the server label the request options still name the outcome.
    expect(answerSummary({ ...answered, optionLabel: undefined })?.choice).toBe('Approve');
    expect(answerSummary(nodeAsk())).toBeNull();
  });

  test('an actor answer shows the chosen label, the confirmation or the text', () => {
    const answered = { state: 'answered', answeredBy: 'alice', delivery: 'delivered' } as const;
    expect(answerSummary(actorAsk({ ...answered, response: 'Postgres' }))).toEqual({
      text: 'Postgres',
    });
    expect(answerSummary(actorAsk({ ...answered, kind: 'confirm', response: 'no' }))).toEqual({
      confirm: 'no',
    });
    expect(
      answerSummary(
        actorAsk({
          ...answered,
          kind: 'choice',
          options: [{ label: 'SQLite', outcome: 'SQLite' }],
          response: 'SQLite',
        })
      )
    ).toEqual({ choice: 'SQLite' });
  });
});

describe('request of the selected node', () => {
  test('the open request of the node wins, then its latest visit; actors never', () => {
    const old = nodeAsk({ id: 'old', sequence: 3, state: 'answered', outcome: 'fail' });
    const open = nodeAsk({ id: 'open', sequence: 7 });
    const later = nodeAsk({ id: 'later', sequence: 5, state: 'answered', outcome: 'fail' });
    expect(nodeRequest([old, open, later], 'decision')?.id).toBe('open');
    expect(nodeRequest([old, later], 'decision')?.id).toBe('later');
    expect(nodeRequest([old, open], 'architecture')).toBeUndefined();
    expect(nodeRequest([actorAsk({ nodeId: 'decision' })], 'decision')).toBeUndefined();
    expect(nodeRequest(null, 'decision')).toBeUndefined();
    expect(nodeRequest([open], undefined)).toBeUndefined();
  });
});

describe('who answers', () => {
  test('owners and editors of an active swarm answer; viewers only read', () => {
    expect(canAnswerRequests(swarm())).toBe(true);
    expect(canAnswerRequests(swarm({ role: 'editor' }))).toBe(true);
    expect(canAnswerRequests(swarm({ role: 'viewer' }))).toBe(false);
    expect(canAnswerRequests(swarm({ state: 'deleting' }))).toBe(false);
    expect(canAnswerRequests(null)).toBe(false);
  });

  test('a failed or unconfirmed delivery of an answered actor request is retried', () => {
    const answered = actorAsk({ state: 'answered', response: 'Postgres' });
    expect(deliveryRetryable({ ...answered, delivery: 'failed' })).toBe(true);
    expect(deliveryRetryable({ ...answered, delivery: 'unconfirmed' })).toBe(true);
    expect(deliveryRetryable({ ...answered, delivery: 'delivered' })).toBe(false);
    expect(deliveryRetryable({ ...answered, delivery: 'pending' })).toBe(false);
    expect(deliveryRetryable({ ...answered, delivery: 'newer-state' })).toBe(false);
    expect(deliveryRetryable(nodeAsk({ state: 'answered', delivery: 'failed' }))).toBe(false);
    expect(deliveryRetryable(nodeAsk({ state: 'answered', delivery: 'unconfirmed' }))).toBe(false);
  });

  // #260: failed did not leave, a retry is safe; unconfirmed left without a
  // receipt and may have reached the actor, so its retry may deliver twice.
  test('only a retry of an unconfirmed delivery may deliver the answer twice', () => {
    const answered = actorAsk({ state: 'answered', response: 'Postgres' });
    expect(retryMayDuplicate({ ...answered, delivery: 'unconfirmed' })).toBe(true);
    expect(retryMayDuplicate({ ...answered, delivery: 'failed' })).toBe(false);
    expect(retryMayDuplicate({ ...answered, delivery: 'delivered' })).toBe(false);
    expect(retryMayDuplicate({ ...answered, delivery: 'pending' })).toBe(false);
    expect(retryMayDuplicate(nodeAsk({ state: 'answered', delivery: 'unconfirmed' }))).toBe(false);
  });
});

describe('API', () => {
  const originalFetch = globalThis.fetch;
  const documentProperty = Object.getOwnPropertyDescriptor(globalThis, 'document');
  const calls: Array<{ url: string; method: string; body?: string }> = [];
  afterEach(() => {
    globalThis.fetch = originalFetch;
    if (documentProperty) Object.defineProperty(globalThis, 'document', documentProperty);
    else Reflect.deleteProperty(globalThis, 'document');
    calls.length = 0;
  });
  const reply = (body: unknown, status = 200) => {
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      calls.push({ url, method: init?.method ?? 'GET', body: init?.body as string | undefined });
      return new Response(JSON.stringify(body), { status });
    }) as typeof fetch;
  };
  const csrf = () =>
    Object.defineProperty(globalThis, 'document', {
      configurable: true,
      value: { cookie: 'csrf=token' },
    });

  test('the inbox reads every request of the swarm by its escaped ID', async () => {
    reply({ results: [nodeAsk(), actorAsk()] });
    const listed = await requestsApi.list('a/b');
    expect(listed.map((request) => request.id)).toEqual(['req-node-3', 'req-actor-1']);
    expect(calls).toEqual([{ url: '/api/swarms/a%2Fb/requests?all=1', method: 'GET' }]);
  });

  test('a malformed request is an error, not a silently missing question', async () => {
    reply({ results: [{ ...nodeAsk(), question: 7 }] });
    await expect(requestsApi.list('s')).rejects.toThrow('Invalid swarm requests response');
    reply({ results: [{ ...nodeAsk(), options: [{ label: 'Approve' }] }] });
    await expect(requestsApi.list('s')).rejects.toThrow('Invalid swarm requests response');
    reply({ requests: [] });
    await expect(requestsApi.list('s')).rejects.toThrow('Invalid swarm requests response');
  });

  test('the answer goes to the new swarm API with exactly outcome and response', async () => {
    csrf();
    reply({ ...nodeAsk(), state: 'answered', outcome: 'success', optionLabel: 'Approve' });
    const answered = await requestsApi.answer('s/1', 'r/1', { outcome: 'success' });
    expect(answered.optionLabel).toBe('Approve');
    expect(calls).toEqual([
      {
        url: '/api/swarms/s%2F1/requests/r%2F1/answer',
        method: 'POST',
        body: JSON.stringify({ outcome: 'success' }),
      },
    ]);
    expect(calls.some((call) => call.url.includes('/api/pipeline-runs/'))).toBe(false);
  });

  test('a retried delivery returns the delivery state and its public reason', async () => {
    csrf();
    reply({
      ...actorAsk(),
      state: 'answered',
      response: 'Postgres',
      delivery: 'failed',
      deliveryError: 'recipient is not connected',
    });
    const retried = await requestsApi.deliver('s', 'r');
    expect(retried).toMatchObject({
      delivery: 'failed',
      deliveryError: 'recipient is not connected',
    });
    expect(calls).toEqual([
      { url: '/api/swarms/s/requests/r/deliver', method: 'POST', body: undefined },
    ]);
  });

  test('an unconfirmed delivery keeps its state and the server reason', async () => {
    csrf();
    const reason =
      'the answer was not sent: the actor session did not receive it; an earlier delivery ' +
      'is unconfirmed: the answer may have reached the actor, and a retry may deliver it twice';
    reply({
      ...actorAsk(),
      state: 'answered',
      response: 'Postgres',
      delivery: 'unconfirmed',
      deliveryError: reason,
    });
    const retried = await requestsApi.deliver('s', 'r');
    expect(retried).toMatchObject({ delivery: 'unconfirmed', deliveryError: reason });
    expect(DELIVERY_STATES).toEqual(['pending', 'delivered', 'failed', 'unconfirmed']);
  });

  test('a refusal carries the server reason', async () => {
    csrf();
    reply({ detail: 'swarm request is no longer open' }, 409);
    await expect(requestsApi.answer('s', 'r', { outcome: 'success' })).rejects.toMatchObject({
      status: 409,
      message: 'swarm request is no longer open',
    });
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
  const owned = (key: string) => key.startsWith('swarm_requests.');

  test('every locale carries every request inbox key with the same interpolations', () => {
    const keys = Object.keys(english).filter(owned);
    expect(keys.length).toBeGreaterThan(25);
    for (const locale of locales) {
      const translated = flatten(read(locale));
      const ownedThere = Object.keys(translated).filter(owned).sort();
      expect({ locale, keys: ownedThere }).toEqual({ locale, keys: [...keys].sort() });
      for (const key of keys) {
        expect({ locale, key, filled: translated[key].trim().length > 0 }).toEqual({
          locale,
          key,
          filled: true,
        });
        expect({ locale, key, tokens: tokens(translated[key]) }).toEqual({
          locale,
          key,
          tokens: tokens(english[key]),
        });
      }
    }
  });

  test('every key the inbox names exists, including each state it can show', () => {
    const files = ['features/swarms/SwarmRequests.tsx'];
    const used = files.flatMap((file) =>
      [
        ...readFileSync(new URL(`../src/${file}`, import.meta.url), 'utf8').matchAll(
          /'(swarm_requests\.[a-z_.]+)'/g
        ),
      ].map((match) => match[1])
    );
    expect(used.length).toBeGreaterThan(15);
    for (const key of used) expect({ key, known: key in english }).toEqual({ key, known: true });
    for (const state of ['open', 'answered', 'cancelled']) {
      expect(english[`swarm_requests.states.${state}`]).toBeTruthy();
    }
    for (const state of DELIVERY_STATES) {
      expect(english[`swarm_requests.delivery_states.${state}`]).toBeTruthy();
    }
    for (const value of ['yes', 'no'])
      expect(english[`swarm_requests.confirm.${value}`]).toBeTruthy();
  });
});
