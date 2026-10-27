import { afterAll, afterEach, beforeAll, describe, expect, spyOn, test } from 'bun:test';
import { apiClient } from '@/services/api/client';
import { BroodAuthError } from '@/services/brood/session';
import { useAuthStore } from '@/stores/useAuthStore';
import { useConfigStore } from '@/stores/useConfigStore';

const spies: Array<{ mockRestore(): void }> = [];
const originalFetchConfig = useConfigStore.getState().fetchConfig;
const originalGlobals = ['localStorage', 'window', 'document', 'fetch'].map(
  (name) => [name, Object.getOwnPropertyDescriptor(globalThis, name)] as const
);
const memory = new Map<string, string>();
const calls: Array<{ url: string; init?: RequestInit }> = [];
let authenticated = false;
let admin = false;
let loginStatus = 204;

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
  define('document', { cookie: 'csrf=fixture-csrf' });
  define('fetch', async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    if (url === '/api/auth/login') {
      authenticated = loginStatus === 204;
      return new Response(
        loginStatus === 204 ? null : JSON.stringify({ error: 'invalid credentials' }),
        { status: loginStatus }
      );
    }
    if (url === '/api/auth/logout') {
      authenticated = false;
      return new Response(null, { status: 204 });
    }
    return Response.json({
      authenticated,
      ...(authenticated
        ? {
            user: { username: 'alice', display_name: 'Alice', is_admin: admin },
          }
        : {}),
    });
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
  authenticated = false;
  admin = false;
  loginStatus = 204;
});

describe('Go web sessions', () => {
  test('ordinary user login bootstraps CSRF and accepts 204 without management privileges', async () => {
    const fetchConfig = spyOn(useConfigStore.getState(), 'fetchConfig').mockRejectedValue(
      new Error('admin only')
    );
    spies.push(fetchConfig);
    await useAuthStore.getState().login({ username: ' alice ', password: 'secret' });
    expect(calls.map((call) => call.url)).toEqual([
      '/api/auth/session',
      '/api/auth/login',
      '/api/auth/session',
    ]);
    expect(calls[1].init?.headers).toEqual({
      'Content-Type': 'application/json',
      'X-CSRFToken': 'fixture-csrf',
    });
    expect(JSON.parse(String(calls[1].init?.body))).toEqual({
      username: 'alice',
      password: 'secret',
    });
    expect(fetchConfig).not.toHaveBeenCalled();
    expect(useAuthStore.getState().isAuthenticated).toBe(true);
    expect(useAuthStore.getState().isSuperuser).toBe(false);
    expect(useAuthStore.getState().username).toBe('alice');
    expect(useAuthStore.getState().managementKey).toBe('');
  });

  test('an administrator can sign in without a configured LLM backend', async () => {
    admin = true;
    const fetchConfig = spyOn(useConfigStore.getState(), 'fetchConfig').mockRejectedValue(
      new Error('unconfigured')
    );
    spies.push(fetchConfig);
    await useAuthStore.getState().login({ username: 'alice', password: 'secret' });
    expect(useAuthStore.getState().isSuperuser).toBe(true);
    expect(fetchConfig).not.toHaveBeenCalled();
  });

  test('a refused login stays signed out', async () => {
    loginStatus = 401;
    await expect(
      useAuthStore.getState().login({ username: 'alice', password: 'wrong' })
    ).rejects.toBeInstanceOf(BroodAuthError);
    expect(useAuthStore.getState().isAuthenticated).toBe(false);
  });

  test('restores actual user flags from the Go session', async () => {
    authenticated = true;
    admin = true;
    expect(await useAuthStore.getState().restoreSession()).toBe(true);
    expect(calls.map((call) => call.url)).toEqual(['/api/auth/session']);
    expect(useAuthStore.getState().isSuperuser).toBe(true);
  });

  test('missing session does not reuse old admin state', async () => {
    useAuthStore.setState({ isAuthenticated: true, isSuperuser: true, username: 'previous' });
    expect(await useAuthStore.getState().checkAuth()).toBe(false);
    expect(useAuthStore.getState().isSuperuser).toBe(false);
    expect(useAuthStore.getState().username).toBe('');
  });
});
