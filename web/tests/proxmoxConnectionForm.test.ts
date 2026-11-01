// FEATURE: Форма настройки пула Proxmox, включая пустую установку и write-only token.
import { afterEach, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import { ProxmoxConnections } from '@/features/substrate/WorkerPoolsPage';
import { ConnectionForm } from '@/features/substrate/ConnectionForm';
import { proxmoxConnectionsApi } from '@/services/brood/substrate';
import en from '../src/i18n/locales/en.json';

const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

async function markup(child: ReturnType<typeof createElement>) {
  Reflect.deleteProperty(globalThis, 'document');
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  return renderToStaticMarkup(createElement(I18nextProvider, { i18n }, child));
}

test('administrators can add the first Proxmox pool to an empty installation', async () => {
  const props = { items: [], error: null, onRetry: () => {} };
  const html = await markup(createElement(ProxmoxConnections, { ...props, canAdd: true }));
  expect(html).toContain('Add Proxmox pool');
  expect(html).not.toContain('role="dialog"');
  const viewer = await markup(createElement(ProxmoxConnections, { ...props, canAdd: false }));
  expect(viewer).toBe('');
});

test('the form renders credential fields, multiline PEM and total resource budget', async () => {
  const html = await markup(createElement(ConnectionForm, { onClose: () => {}, refresh: async () => {} }));
  for (const label of Object.values(en.proxmox_connections.fields)) expect(html).toContain(label);
  expect(html).toContain('type="password"');
  expect(html).toContain('<textarea');
  expect(html).toContain('name="connection-caPEM"');
  expect(html).toContain('name="connection-templateDigest"');
  expect(html).toContain('name="connection-memoryGiB"');
  expect(html).toContain('disabled=""');
});

test('the client posts credentials with CSRF and has no secret readback endpoint', async () => {
  Object.defineProperty(globalThis, 'document', { configurable: true, value: { cookie: 'csrf=pool-form-test' } });
  let called = false;
  globalThis.fetch = (async (url, options) => {
    called = true;
    expect(url).toBe('/api/proxmox-connections');
    expect(options).toMatchObject({ method: 'POST', credentials: 'same-origin', headers: { 'X-CSRFToken': 'pool-form-test' } });
    expect(JSON.parse(options!.body as string).tokenSecret).toBe('synthetic-secret');
    return Response.json({ uid: 'c1', credentialConfigured: true }, { status: 201 });
  }) as typeof fetch;
  const result = await proxmoxConnectionsApi.create({ name: 'pve', endpoint: 'https://pve.invalid:8006',
    tokenID: 'a@pve!b', tokenSecret: 'synthetic-secret', caPEM: 'pem', node: 'n', resourcePool: 'r', storage: 's', network: 'v' });
  expect(called).toBe(true);
  expect(result.tokenSecret).toBeUndefined();
});
