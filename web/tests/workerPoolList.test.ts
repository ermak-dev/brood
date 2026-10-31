// FEATURE: WorkerPool lists use the Kubernetes items envelope.
// SPEC: Existing pools must appear; malformed responses must not look empty.
import { afterEach, expect, test } from 'bun:test';
import { workerPoolsApi, type ProxmoxPool } from '@/services/brood/substrate';
import * as substrate from '@/services/brood/substrate';
import * as poolPage from '@/features/substrate/WorkerPoolsPage';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import { readFileSync } from 'node:fs';

// FEATURE: Отдельный read-only блок подключений Proxmox на странице пулов.
// SPEC: items, независимость от Kubernetes, без секретов и ложной готовности;
// пустой список скрыт, отказ локален, все шесть локалей имеют одинаковые ключи.
const connection = {
  name: 'brood-proxmox', endpoint: 'https://pve.example.invalid:8006',
  tokenID: 'synthetic-user@pve!panel', node: 'synthetic-node',
  resourcePool: 'synthetic-resource-pool', storage: 'synthetic-storage',
  network: 'synthetic-network', credentialConfigured: true, runtimeAvailable: false,
} satisfies ProxmoxPool;
const locales = ['en', 'ru', 'zh-CN', 'zh-TW', 'vi', 'ko'];
const messages = (locale: string) => JSON.parse(readFileSync(
  new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8'
));

async function connectionMarkup(items: ProxmoxPool[] | null, error: string | null = null, locale = 'en') {
  expect(typeof poolPage.ProxmoxConnections).toBe('function');
  const i18n = createInstance();
  await i18n.init({ lng: locale, resources: { [locale]: { translation: messages(locale) } } });
  return renderToStaticMarkup(createElement(I18nextProvider, { i18n },
    createElement(poolPage.ProxmoxConnections, { items, error, onRetry: () => {} })
  ));
}

test('Proxmox reads its own items envelope without requiring a native WorkerPool', async () => {
  expect(substrate.proxmoxPoolsApi).toBeDefined();
  const calls: Array<[unknown, unknown]> = [];
  globalThis.fetch = (async (url, options) => {
    calls.push([url, options]);
    return Response.json({ items: [connection] });
  }) as typeof fetch;
  expect(await substrate.proxmoxPoolsApi.list()).toEqual([connection]);
  expect(calls).toEqual([['/api/proxmox-pools', {
    method: 'GET', credentials: 'same-origin', headers: {}, body: undefined,
  }]]);
});

test('Proxmox empty items stay empty, malformed data and 404 never invent connections', async () => {
  expect(substrate.proxmoxPoolsApi).toBeDefined();
  reply({ items: [] });
  expect(await substrate.proxmoxPoolsApi.list()).toEqual([]);
  for (const invalid of [{ results: [connection] }, { items: null }, { items: 'wrong' }, null]) {
    reply(invalid);
    await expect(substrate.proxmoxPoolsApi.list()).rejects.toThrow('invalid Proxmox connection list');
  }
  for (const status of [403, 404, 503]) {
    globalThis.fetch = (async () => Response.json({ detail: 'synthetic failure' }, { status })) as typeof fetch;
    await expect(substrate.proxmoxPoolsApi.list()).rejects.toMatchObject({ status });
  }
});

test('connection block is read-only, reports configuration not readiness, and omits token data', async () => {
  const withUnexpectedSecret = { ...connection, tokenSecret: 'SYNTHETIC_SECRET_SENTINEL' };
  const markup = await connectionMarkup([withUnexpectedSecret]);
  for (const value of [connection.name, connection.endpoint, connection.node, connection.resourcePool,
    connection.storage, connection.network, 'Credential configured', 'Yes', 'Runtime not installed']) {
    expect(markup).toContain(value);
  }
  for (const forbidden of ['SYNTHETIC_SECRET_SENTINEL', connection.tokenID, '<input', '<button',
    'tabindex=', 'Ready', 'replicas']) expect(markup).not.toContain(forbidden);
  expect(await connectionMarkup([{ ...connection, credentialConfigured: false }])).toContain('No');
});

test('empty and loading connections add no clutter; failure is concise and scoped', async () => {
  expect(await connectionMarkup([])).toBe('');
  expect(await connectionMarkup(null)).toBe('');
  const markup = await connectionMarkup(null, 'SYNTHETIC_SECRET_SENTINEL');
  expect(markup).toContain('Could not load Proxmox connections');
  expect(markup).toContain('Retry');
  expect(markup).not.toContain('SYNTHETIC_SECRET_SENTINEL');
  expect(markup).not.toContain(connection.name);
});

test('all six locales translate the connection block with matching keys and booleans', async () => {
  const keys = ['title', 'endpoint', 'node', 'resource_pool', 'storage', 'network',
    'credential_configured', 'yes', 'no', 'runtime_not_installed', 'load_failed'];
  for (const locale of locales) {
    const strings = messages(locale).proxmox_connections;
    expect(strings).toBeDefined();
    expect(Object.keys(strings).sort()).toEqual([...keys].sort());
    for (const value of Object.values(strings)) expect(typeof value === 'string' && value.length > 0).toBe(true);
    const markup = await connectionMarkup([connection], null, locale);
    expect(markup).toContain(strings.runtime_not_installed);
    expect(markup).toContain(strings.yes);
    expect(await connectionMarkup([{ ...connection, credentialConfigured: false }], null, locale)).toContain(strings.no);
  }
});

test('existing admin pools page mounts an independent connection resource outside the pool result branches', () => {
  const source = readFileSync(new URL('../src/features/substrate/WorkerPoolsPage.tsx', import.meta.url), 'utf8');
  expect(source).toContain('useResource(proxmoxPoolsApi.list, { headerRefresh: false })');
  expect(source).toContain('<ProxmoxConnections');
  expect(source).toContain('items={connections.data}');
  expect(source).toContain('error={connections.error}');
  expect(source).toContain('onRetry={connections.refresh}');
  const routes = readFileSync(new URL('../src/router/MainRoutes.tsx', import.meta.url), 'utf8');
  expect(routes).toContain("{ path: '/worker-pools', element: <WorkerPoolsPage /> }");
});

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

function reply(value: unknown) {
  globalThis.fetch = (async () => Response.json(value)) as typeof fetch;
}

test('reads existing pools from items, not the ateapi results envelope', async () => {
  const pool = { metadata: { name: 'development' }, spec: { replicas: 2 } };
  reply({ items: [pool] });
  expect(await workerPoolsApi.list()).toEqual([pool]);
});

test('accepts an empty or null Kubernetes collection', async () => {
  reply({ items: [] });
  expect(await workerPoolsApi.list()).toEqual([]);
  reply({ items: null });
  expect(await workerPoolsApi.list()).toEqual([]);
});

test('rejects a malformed collection rather than hiding all pools', async () => {
  for (const invalid of [{ results: [] }, { items: 'wrong' }, null]) {
    reply(invalid);
    await expect(workerPoolsApi.list()).rejects.toThrow('invalid WorkerPool list');
  }
});
