import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import type { AxiosAdapter } from 'axios';
import { apiClient } from '../src/services/api/client';

const originalWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
// В браузере cookie есть всегда: запись берёт из неё csrf.
beforeEach(() => setCookie('csrf=fixture-csrf'));
afterEach(() => {
  apiClient.setConfig({ apiBase: '', managementKey: '' });
  if (originalWindow) Object.defineProperty(globalThis, 'window', originalWindow);
  else Reflect.deleteProperty(globalThis, 'window');
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

function setCookie(cookie: string): void {
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { cookie } });
}

// Middleware сессий Go отказывает всем методам, кроме GET и HEAD, если
// X-CSRFToken не равен cookie csrf сессии; management-прокси тоже (#227).
describe('session CSRF on the management transport', () => {
  const sent: Array<[string | undefined, unknown]> = [];
  const adapter: AxiosAdapter = async (config) => {
    sent.push([config.method, config.headers['X-CSRFToken']]);
    return { data: {}, status: 200, statusText: 'OK', headers: {}, config };
  };
  afterEach(() => {
    sent.length = 0;
  });

  test('writes send the csrf cookie as X-CSRFToken, reads do not', async () => {
    setCookie('lang=en; csrf=session-csrf');
    apiClient.setConfig({ apiBase: 'https://brood.invalid', managementKey: '' });
    await apiClient.post('/oauth-model-alias', {}, { adapter });
    await apiClient.put('/oauth-model-alias', {}, { adapter });
    await apiClient.patch('/oauth-model-alias', {}, { adapter });
    await apiClient.delete('/oauth-model-alias', { adapter });
    await apiClient.postForm('/auth-files', new FormData(), { adapter });
    await apiClient.captureConnection().delete('/oauth/session', { adapter });
    await apiClient.get('/oauth-model-alias', { adapter });
    await apiClient.getRaw('/auth-files/download', { adapter });
    expect(sent).toEqual([
      ['post', 'session-csrf'],
      ['put', 'session-csrf'],
      ['patch', 'session-csrf'],
      ['delete', 'session-csrf'],
      ['post', 'session-csrf'],
      ['delete', 'session-csrf'],
      ['get', undefined],
      ['get', undefined],
    ]);
  });

  test('reads the token per request, so a login rotation in another tab is picked up', async () => {
    apiClient.setConfig({ apiBase: 'https://brood.invalid', managementKey: '' });
    setCookie('csrf=before-login');
    await apiClient.put('/oauth-model-alias', {}, { adapter });
    setCookie('csrf=after-login');
    await apiClient.put('/oauth-model-alias', {}, { adapter });
    expect(sent.map(([, token]) => token)).toEqual(['before-login', 'after-login']);
  });
});

describe('v8 management transport', () => {
  test('uses the v8 prefix, bearer authentication and bare JSON scalar writes', async () => {
    apiClient.setConfig({
      apiBase: 'https://proxy.invalid/gateway/v8/management',
      managementKey: 'fixture-only',
    });
    const adapter: AxiosAdapter = async (config) => {
      expect(config.baseURL).toBe('https://proxy.invalid/gateway/v8/management');
      expect(config.url).toBe('/config/observability/logs/debug');
      expect(config.headers.Authorization).toBe('Bearer fixture-only');
      expect(config.data).toBe('false');
      return { data: { status: 'ok' }, status: 200, statusText: 'OK', headers: {}, config };
    };
    await apiClient.put('/config/observability/logs/debug', false, { adapter });
  });

  test('retains backend version and plugin support events', async () => {
    const events: Event[] = [];
    Object.defineProperty(globalThis, 'window', {
      configurable: true,
      value: {
        dispatchEvent: (event: Event) => {
          events.push(event);
          return true;
        },
      },
    });
    apiClient.setConfig({ apiBase: 'http://proxy.invalid', managementKey: 'fixture-only' });
    const adapter: AxiosAdapter = async (config) => ({
      data: {},
      status: 200,
      statusText: 'OK',
      config,
      headers: { 'x-cpa-version': 'v8.0.3', 'x-cpa-support-plugin': 'true' },
    });
    await apiClient.get('/config', { adapter });
    expect(events.map((event) => event.type)).toEqual([
      'server-version-update',
      'server-plugin-support-update',
    ]);
    expect((events[0] as CustomEvent).detail.version).toBe('v8.0.3');
    expect((events[1] as CustomEvent).detail.supportsPlugin).toBe(true);
  });
});

test('captured SIWC transport keeps original credentials and suppresses stale events, including ABA', async () => {
  const events: Event[] = [];
  Object.defineProperty(globalThis, 'window', {
    configurable: true,
    value: {
      dispatchEvent: (event: Event) => {
        events.push(event);
        return true;
      },
    },
  });
  apiClient.setConfig({ apiBase: 'https://original.invalid', managementKey: 'original-fixture' });
  const captured = apiClient.captureConnection();
  apiClient.setConfig({ apiBase: 'https://new.invalid', managementKey: 'new-fixture' });
  const adapter: AxiosAdapter = async (config) => {
    expect(config.baseURL).toBe('https://original.invalid/v8/management');
    expect(config.headers.Authorization).toBe('Bearer original-fixture');
    return {
      data: {},
      status: 200,
      statusText: 'OK',
      config,
      headers: { 'x-cpa-version': 'old', 'x-cpa-support-plugin': 'true' },
    };
  };
  await captured.delete('/oauth/session', { params: { state: 'original-session' }, adapter });
  apiClient.setConfig({ apiBase: 'https://original.invalid', managementKey: 'original-fixture' });
  await captured.delete('/oauth/session', { params: { state: 'original-session' }, adapter });
  expect(events).toEqual([]);
  const unauthorized: AxiosAdapter = async (config) => {
    throw { isAxiosError: true, message: 'Unauthorized', config, response: { status: 401 } };
  };
  await expect(captured.delete('/oauth/session', { adapter: unauthorized })).rejects.toThrow();
  expect(events).toEqual([]);
});
