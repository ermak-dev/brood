// FEATURE: История детей и подтверждённый abort только неполной незапущенной группы.
// SPEC: Исторические личности неизменны; состояние доставки не означает успех;
// viewer, running/completed и неопределённые эффекты не получают abort.
import { afterEach, describe, expect, test } from 'bun:test';
import { childrenApi, parseChildGroup, parseChildGroupPage } from '@/services/brood/children';
import { canAbortChildGroup } from '@/features/swarms/childState';
import { childGroup, childSummary, childTask } from './fixtures/swarmChildren';

describe('child history normalization', () => {
  test('preserves report text and original executor incarnation without live actor lookup', () => {
    const report = 'Independent <script>not executable</script>\nReport verified.';
    const group = childGroup({
      interrupted: true,
      readiness: 'unconfirmed',
      started: 1,
      open: 1,
      children: [
        childTask({ state: 'completed', delivery: 'unconfirmed', outcome: 'success', report }),
      ],
    });
    expect(parseChildGroup(group)).toEqual(group);
    expect(parseChildGroup(group).children[0].executorIncarnation).toBe(4);
  });

  test('rejects malformed counts, identities, reports, and duplicate child IDs', () => {
    for (const group of [
      childGroup({ total: 65 }),
      childGroup({ parentSequence: Number.MAX_SAFE_INTEGER + 1 }),
      childGroup({ pass: 4 }),
      childGroup({ started: 2 }),
      childGroup({ registered: 0 }),
      childGroup({ registered: 2, children: [childTask(), childTask()] }),
      { ...childGroup(), readiness: null },
      { ...childGroup(), children: [{ ...childTask(), executorIncarnation: 1.5 }] },
      { ...childGroup(), children: [{ ...childTask(), report: 17 }] },
    ]) {
      expect(() => parseChildGroup(group)).toThrow('Invalid child history response');
    }
  });

  test('pages preserve descending sequence and drop unexpected payload fields', () => {
    const body = {
      items: [
        { ...childSummary({ parentSequence: 5 }), children: [childTask()] },
        childSummary({ parentSequence: 3 }),
      ],
      nextBefore: 3,
    };
    const page = parseChildGroupPage(body);
    expect(page.items.map((item) => item.parentSequence)).toEqual([5, 3]);
    expect(page.nextBefore).toBe(3);
    expect(Object.hasOwn(page.items[0], 'children')).toBe(false);
    for (const invalid of [
      { items: body.items, nextBefore: 5 },
      { items: [...body.items].reverse(), nextBefore: 5 },
      { items: [childSummary(), childSummary()], nextBefore: 0 },
      { items: [], nextBefore: 3 },
      { items: Array.from({ length: 101 }, () => childSummary()), nextBefore: 0 },
    ]) {
      expect(() => parseChildGroupPage(invalid)).toThrow('Invalid child history response');
    }
  });
});

describe('unstarted group recovery eligibility', () => {
  test('only owner/admin may abort an incomplete known-unsent group', () => {
    expect(canAbortChildGroup(childGroup(), true)).toBe(true);
    expect(canAbortChildGroup(childGroup({ interrupted: true }), true)).toBe(true);
    expect(canAbortChildGroup(childGroup(), false)).toBe(false);
    expect(canAbortChildGroup(childGroup({ registered: 0, children: [] }), true)).toBe(true);
  });

  test('full groups, running/completed children and uncertainty remain held', () => {
    for (const group of [
      childGroup({ state: 'aborted', open: 0 }),
      childGroup({ state: 'future-state' }),
      childGroup({ revision: 0 }),
      childGroup({ revision: 1.5 }),
      childGroup({ revision: Number.MAX_SAFE_INTEGER }),
      childGroup({ registered: 2, children: [childTask(), childTask({ id: 'child-2' })] }),
      childGroup({ started: 1 }),
      childGroup({ children: [childTask({ state: 'running' })] }),
      childGroup({ children: [childTask({ state: 'completed', outcome: 'success' })] }),
      childGroup({ children: [childTask({ state: 'future-state' })] }),
      childGroup({ children: [childTask({ delivery: 'sending' })] }),
      childGroup({ children: [childTask({ delivery: 'unconfirmed' })] }),
      childGroup({ children: [] }),
    ]) {
      expect(canAbortChildGroup(group, true)).toBe(false);
    }
  });
});

describe('same-origin child API', () => {
  const originalFetch = globalThis.fetch;
  const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
  afterEach(() => {
    globalThis.fetch = originalFetch;
    if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
    else Reflect.deleteProperty(globalThis, 'document');
  });

  test('encodes swarm scope and sends exactly revision and explicit confirmation', async () => {
    const calls: Array<{ url: string; method: string; body: unknown }> = [];
    Object.defineProperty(globalThis, 'document', {
      configurable: true,
      value: { cookie: 'csrf=fixture-csrf' },
    });
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      calls.push({
        url,
        method: init?.method ?? '',
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      expect(init?.credentials).toBe('same-origin');
      if (init?.method === 'POST') {
        expect(new Headers(init.headers).get('X-CSRFToken')).toBe('fixture-csrf');
        return Response.json({ state: 'aborted', revision: 2, open: 0 });
      }
      return Response.json(
        url.includes('?') ? { items: [childSummary()], nextBefore: 0 } : childGroup()
      );
    }) as typeof fetch;
    await childrenApi.list('swarm/one', 0, 25);
    await childrenApi.group('swarm/one', 3);
    await childrenApi.abort('swarm/one', 3, 1);
    expect(calls).toEqual([
      { url: '/api/swarms/swarm%2Fone/children?before=0&limit=25', method: 'GET', body: null },
      { url: '/api/swarms/swarm%2Fone/children/3', method: 'GET', body: null },
      {
        url: '/api/swarms/swarm%2Fone/children/3/abort',
        method: 'POST',
        body: { revision: 1, confirm: true },
      },
    ]);
  });

  test('never retries an ambiguous abort or treats an invalid reply as success', async () => {
    Object.defineProperty(globalThis, 'document', {
      configurable: true,
      value: { cookie: 'csrf=x' },
    });
    let writes = 0;
    globalThis.fetch = (async () => {
      writes++;
      throw new TypeError('synthetic transport failure');
    }) as typeof fetch;
    await expect(childrenApi.abort('s', 3, 1)).rejects.toThrow('synthetic transport failure');
    expect(writes).toBe(1);
    for (const body of [
      { state: 'open', revision: 1, open: 1 },
      { state: 'aborted', revision: 9, open: 0 },
    ]) {
      globalThis.fetch = (async () => Response.json(body)) as typeof fetch;
      await expect(childrenApi.abort('s', 3, 1)).rejects.toThrow('Invalid child history response');
    }
  });

  test('invalid cursors or sequences make no request', async () => {
    let calls = 0;
    globalThis.fetch = (async () => {
      calls++;
      return Response.json({});
    }) as typeof fetch;
    await expect(childrenApi.list('s', -1)).rejects.toThrow();
    await expect(childrenApi.list('s', 0, 101)).rejects.toThrow();
    await expect(childrenApi.group('s', 0)).rejects.toThrow();
    await expect(childrenApi.abort('s', 3, 0)).rejects.toThrow();
    expect(calls).toBe(0);
  });
});
