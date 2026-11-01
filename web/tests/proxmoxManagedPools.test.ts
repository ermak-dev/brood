// FEATURE: Управляемые пулы Proxmox без подмены native WorkerPool.
// SPEC: Точные activate/drain/enable endpoints, пустое тело, decimal-string
// capacity; runtime configuration не означает готовность процесса.
// IMPLEMENTATION: Типизированные действия с CSRF, подтверждения и строки резервирования.
// RESULT: IMPLEMENTED — контракт пулов проверен без подмены native WorkerPool.
// STATUS: DONE
import { afterEach, beforeEach, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import { proxmoxPoolsApi, type ProxmoxPool } from '@/services/brood/substrate';
import { ProxmoxConnections } from '@/features/substrate/WorkerPoolsPage';
import en from '../src/i18n/locales/en.json';

const connection = {
  name: 'limited', endpoint: 'https://pve.example.invalid:8006', tokenID: 'synthetic@pve!test',
  node: 'pve', resourcePool: 'test', storage: 'test', network: 'isolated',
  credentialConfigured: true, runtimeAvailable: true,
};
const allocation = {
  uid: 'pool-uid', state: 'enabled', version: '1', maxActors: '2', usedActors: '0',
  cpuMillicores: '2000', usedCPUMillicores: '0', memoryBytes: '9007199254740993',
  usedMemoryBytes: '0', diskBytes: '2147483648', usedDiskBytes: '0',
};
const originalFetch = globalThis.fetch;
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
beforeEach(() => {
  Object.defineProperty(globalThis, 'document', {
    configurable: true, value: { cookie: 'csrf=managed-pool-test' },
  });
});
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

async function markup(pool: ProxmoxPool) {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  return renderToStaticMarkup(createElement(I18nextProvider, { i18n },
    createElement(ProxmoxConnections, { items: [pool], error: null, onRetry: () => {} })
  ));
}

test('pool actions use encoded connection names and no operator configuration', async () => {
  const calls: Array<[unknown, unknown]> = [];
  globalThis.fetch = (async (url, options) => {
    calls.push([url, options]);
    return Response.json({ managedPool: allocation });
  }) as typeof fetch;
  for (const action of ['activate', 'drain', 'enable'] as const) {
    expect(typeof proxmoxPoolsApi[action]).toBe('function');
    expect(await proxmoxPoolsApi[action]('a/b')).toEqual({ managedPool: allocation });
    const [url, options] = calls.at(-1)!;
    expect(url).toBe(`/api/proxmox-pools/a%2Fb/${action}`);
    expect(options).toMatchObject({ method: 'POST', credentials: 'same-origin',
      headers: { 'X-CSRFToken': 'managed-pool-test' } });
    expect((options as RequestInit).body).toBeUndefined();
  }
});

test('activation is explicit and only offered for installed runtime', async () => {
  const installed = await markup(connection);
  expect(installed).toContain('Activate pool');
  expect(installed).not.toContain('Runtime not installed');
  expect(installed).not.toContain('Ready');
  const uninstalled = await markup({ ...connection, runtimeAvailable: false });
  expect(uninstalled).toContain('Runtime not installed');
  expect(uninstalled).not.toContain('Activate pool');
});

test('managed allocation is real reservation data, including zero and exact int64 strings', async () => {
  const active = await markup({ ...connection, managedPool: allocation });
  expect(active).toContain('Enabled');
  expect(active).toContain('Drain pool');
  expect(active).not.toContain('Enable pool');
  expect(active).toContain('0 / 2');
  expect(active).toContain('0 / 9007199254740993');
  expect(active).not.toContain('9007199254740992');
  expect(active).not.toContain('Activate pool');
  expect(active).not.toContain('Runtime not installed');
  const draining = await markup({ ...connection, managedPool: { ...allocation, state: 'draining' } });
  expect(draining).toContain('Draining');
  expect(draining).toContain('Enable pool');
  expect(draining).not.toContain('Drain pool');
  const unavailable = await markup({ ...connection, runtimeAvailable: false,
    managedPool: { ...allocation, state: 'draining' } });
  expect(unavailable).not.toContain('Enable pool');
  expect(unavailable).toContain('0 / 2');
});

test('server refuses activation visibly rather than synthesizing readiness', async () => {
  globalThis.fetch = (async () => Response.json({ detail: 'installed runtime unavailable' }, { status: 409 })) as typeof fetch;
  expect(typeof proxmoxPoolsApi.activate).toBe('function');
  await expect(proxmoxPoolsApi.activate('limited')).rejects.toMatchObject({ status: 409, message: 'installed runtime unavailable' });
});
