import { afterEach, describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import { ActorActionFailure } from '@/features/substrate/components/ActorActionFailure';
import en from '@/i18n/locales/en.json';
import { BroodApiError, request } from '@/services/brood/http';
import { placementFailure } from '@/features/substrate/placementFailure';

// BUG: Отказ запуска пропадал в уведомлении, а HTTP-клиент терял структурную причину (#90).
// SPEC: Точный code отличает placement от других ошибок; неизвестные и повреждённые
// ответы не становятся успешной диагностикой и не ломают экран.
const body = { code: 'actor_placement_unavailable', detail: 'No worker can host this actor.', placement: {
  noWorkers: false, templateSelector: { purpose: 'development' }, actorSelector: null,
  workers: [{ name: 'worker-one', reasons: [{ code: 'worker is not active', state: 'WORKER_STATE_DRAINING' }, { code: 'insufficient CPU', required: '4000', available: '0' }] }],
} };
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

describe('placement failure view', () => {
  test('member markup never includes fleet details even if they are present in a response', async () => {
    const i18n = createInstance();
    await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
    const render = (administrator: boolean) => renderToStaticMarkup(
      createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null,
        createElement(ActorActionFailure, { name: 'blocked', administrator, error: new BroodApiError(409, body.detail, body), onClose: () => {} })))
    );
    const member = render(false);
    expect(member).toContain('role="alert"');
    expect(member).toContain('Ask an administrator');
    expect(member).not.toContain('worker-one');
    expect(member).not.toContain('/worker-pools');
    const admin = render(true);
    expect(admin).toContain('worker-one');
    expect(admin).toContain('available: 0');
    expect(admin).toContain('development');
    expect(admin).toContain('/worker-pools');
  });
});

describe('placement refusal payload', () => {
  test('the HTTP client retains the structured response with the original status', async () => {
    globalThis.fetch = (async () => new Response(JSON.stringify(body), { status: 409 })) as typeof fetch;
    let caught: unknown;
    try { await request('GET', '/api/example'); } catch (error) { caught = error; }
    expect(caught).toBeInstanceOf(BroodApiError);
    expect(placementFailure(caught)?.diagnostic?.workers[0].reasons[1].available).toBe('0');
    expect((caught as BroodApiError).status).toBe(409);
  });

  test('a member refusal is meaningful without any fleet details', () => {
    const error = new BroodApiError(409, 'Ask an administrator', { code: body.code });
    expect(placementFailure(error)).toEqual({ diagnostic: null });
  });

  test('only the stable code identifies a placement refusal', () => {
    expect(placementFailure(new Error(body.detail))).toBeNull();
    expect(placementFailure(new BroodApiError(409, body.detail))).toBeNull();
    expect(placementFailure(new BroodApiError(409, body.detail, { ...body, code: 'another_error' }))).toBeNull();
  });

  test('untrusted malformed details fall back to a generic placement refusal', () => {
    for (const placement of [null, [], { workers: 'wrong' }, { workers: [{ name: 1, reasons: [] }] }, { workers: [{ name: 'worker', reasons: [{}] }] }]) {
      expect(placementFailure(new BroodApiError(409, body.detail, { ...body, placement })))
        .toEqual({ diagnostic: null });
    }
  });

  test('selectors and advisory numeric strings are preserved', () => {
    const parsed = placementFailure(new BroodApiError(409, body.detail, body));
    expect(parsed?.diagnostic?.templateSelector).toEqual({ purpose: 'development' });
    expect(parsed?.diagnostic?.actorSelector).toEqual({});
    expect(parsed?.diagnostic?.workers[0].reasons[0].state).toBe('WORKER_STATE_DRAINING');
  });
});
