import { afterAll, afterEach, beforeAll, describe, expect, spyOn, test } from 'bun:test';
import { apiClient } from '@/services/api/client';
import { BroodAuthError } from '@/services/brood/session';
import { useAuthStore } from '@/stores/useAuthStore';
import { useConfigStore } from '@/stores/useConfigStore';

// brood fork: the panel signs in to the brood server, and the management key never
// reaches the browser. These checks replace upstream's legacy-backend login diagnostic,
// which probed /v0 with the management key the fork no longer has.

const spies: Array<{ mockRestore(): void }> = [];
const originalFetchConfig = useConfigStore.getState().fetchConfig;
const originalGlobals = ['localStorage', 'window', 'document', 'fetch'].map(
  (name) => [name, Object.getOwnPropertyDescriptor(globalThis, name)] as const
);
const memory = new Map<string, string>();
const calls: Array<{ url: string; init?: RequestInit }> = [];
let answer: (url: string) => Response = () => new Response('{}');

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status });
}

beforeAll(() => {
  const define = (name: string, value: unknown) =>
    Object.defineProperty(globalThis, name, { configurable: true, value });
  define('localStorage', {
    getItem: (key: string) => memory.get(key) ?? null,
    setItem: (key: string, value: string) => memory.set(key, value),
    removeItem: (key: string) => memory.delete(key),
  });
  define('window', {
    location: { protocol: 'https:', hostname: 'brood.invalid', port: '' },
    addEventListener: () => undefined,
    dispatchEvent: () => true,
  });
  define('document', { cookie: 'csrftoken=fixture-csrf' });
  define('fetch', async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return answer(url);
  });
});
afterAll(() => {
  for (const [name, descriptor] of originalGlobals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
});
afterEach(() => {
  spies.splice(0).forEach((spy) => spy.mockRestore());
  useConfigStore.setState({ fetchConfig: originalFetchConfig });
  apiClient.setConfig({ apiBase: '', managementKey: '' });
  useAuthStore.setState({ isAuthenticated: false, connectionStatus: 'disconnected' });
  calls.splice(0);
});

describe('brood sign-in', () => {
  test('signs in to the brood server, then reaches the management API of this origin', async () => {
    answer = () => json(200, { authenticated: true, username: 'admin', is_superuser: true });
    const fetchConfig = spyOn(useConfigStore.getState(), 'fetchConfig').mockResolvedValue(
      {} as never
    );
    spies.push(fetchConfig);

    await useAuthStore.getState().login({ username: ' admin ', password: 'secret' });

    expect(calls[0].url).toBe('/auth/login');
    expect(calls[0].init?.method).toBe('POST');
    expect(calls[0].init?.headers).toEqual({
      'Content-Type': 'application/json',
      'X-CSRFToken': 'fixture-csrf',
    });
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({
      username: 'admin',
      password: 'secret',
    });
    expect(fetchConfig).toHaveBeenCalledTimes(1);
    const state = useAuthStore.getState();
    expect(state.isAuthenticated).toBe(true);
    expect(state.apiBase).toBe('https://brood.invalid');
    expect(state.managementKey).toBe('');
  });

  test('a refused sign-in never touches the management API', async () => {
    answer = () => json(401, { error: 'invalid username or password' });
    const fetchConfig = spyOn(useConfigStore.getState(), 'fetchConfig').mockResolvedValue(
      {} as never
    );
    spies.push(fetchConfig);

    const attempt = useAuthStore.getState().login({ username: 'admin', password: 'wrong' });
    await expect(attempt).rejects.toBeInstanceOf(BroodAuthError);
    expect(fetchConfig).not.toHaveBeenCalled();
    expect(useAuthStore.getState().isAuthenticated).toBe(false);
  });

  test('restores a live brood session without asking for credentials', async () => {
    answer = () => json(200, { authenticated: true, username: 'admin', is_superuser: true });
    spies.push(
      spyOn(useConfigStore.getState(), 'fetchConfig').mockResolvedValue({} as never)
    );

    expect(await useAuthStore.getState().restoreSession()).toBe(true);
    expect(calls.map((call) => call.url)).toEqual(['/auth/session']);
    expect(useAuthStore.getState().isAuthenticated).toBe(true);
  });

  test('without a brood session the panel stays signed out', async () => {
    answer = () => json(200, { authenticated: false, username: '', is_superuser: false });
    const fetchConfig = spyOn(useConfigStore.getState(), 'fetchConfig').mockResolvedValue(
      {} as never
    );
    spies.push(fetchConfig);

    expect(await useAuthStore.getState().restoreSession()).toBe(false);
    expect(fetchConfig).not.toHaveBeenCalled();
  });
});
