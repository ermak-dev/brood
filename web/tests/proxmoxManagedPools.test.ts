// FEATURE: Управляемые пулы Proxmox без подмены native WorkerPool.
// SPEC: Точные activate/drain/enable endpoints, пустое тело, decimal-string
// capacity; runtime configuration не означает готовность процесса.
// IMPLEMENTATION: Типизированные действия с CSRF, подтверждения и строки резервирования.
// Строка таблицы только открывает панель пула; действие по состоянию -- в ней (#157).
// RESULT: IMPLEMENTED — контракт пулов проверен без подмены native WorkerPool.
// STATUS: DONE
import { afterEach, beforeEach, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import type { TFunction } from 'i18next';
import { proxmoxPoolsApi, type ProxmoxPool } from '@/services/brood/substrate';
import { ProxmoxConnections, ProxmoxPoolDetails } from '@/features/substrate/WorkerPoolsPage';
import { proxmoxPoolActionGroups } from '@/features/substrate/poolActions';
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

async function translator() {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  return i18n;
}

async function markup(pool: ProxmoxPool) {
  const i18n = await translator();
  return renderToStaticMarkup(createElement(I18nextProvider, { i18n },
    createElement(ProxmoxConnections, { items: [pool], error: null, onRetry: () => {} })
  ));
}

async function details(pool: ProxmoxPool) {
  const i18n = await translator();
  return renderToStaticMarkup(createElement(I18nextProvider, { i18n },
    createElement(ProxmoxPoolDetails, { pool })
  ));
}

/** Действия панели пула в английском тексте: метка, причина блокировки, есть ли обработчик. */
async function offered(pool: ProxmoxPool) {
  const i18n = await translator();
  return proxmoxPoolActionGroups(pool, {
    t: i18n.t.bind(i18n) as TFunction, pending: false, confirm: () => {}, perform: async () => {},
  }).flatMap((group) => group.actions.map((action) => ({
    label: action.label, blocked: action.blocked, selectable: Boolean(action.onSelect),
  })));
}

const ACTION_LABELS = ['Activate pool', 'Drain pool', 'Enable pool'];

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
  expect(installed).not.toContain('Runtime not installed');
  expect(installed).not.toContain('Ready');
  expect(await offered(connection)).toEqual([{ label: 'Activate pool', blocked: undefined, selectable: true }]);
  const uninstalled = await markup({ ...connection, runtimeAvailable: false });
  expect(uninstalled).toContain('Runtime not installed');
  // Без runtime активация видна в панели, но не нажимается и называет причину.
  expect(await offered({ ...connection, runtimeAvailable: false })).toEqual([{
    label: 'Activate pool', blocked: en.proxmox_connections.runtime_not_installed, selectable: false,
  }]);
  // Строка таблицы не несёт ни одного действия: они только в панели.
  for (const html of [installed, uninstalled]) {
    for (const label of ACTION_LABELS) expect(html).not.toContain(label);
    expect(html).not.toContain('<button');
  }
});

test('managed allocation is real reservation data, including zero and exact int64 strings', async () => {
  const enabled = { ...connection, managedPool: allocation };
  const active = await markup(enabled);
  expect(active).toContain('Enabled');
  expect(active).toContain('0 / 2');
  expect(await offered(enabled)).toEqual([{ label: 'Drain pool', blocked: undefined, selectable: true }]);
  // Service values are hidden: capacity is human units, never raw int64 bytes.
  for (const html of [active, await details(enabled)]) {
    expect(html).toContain('0 / 2');
    expect(html).not.toContain('9007199254740993');
    expect(html).not.toContain('9007199254740992');
    expect(html).not.toContain('Credential configured');
    expect(html).not.toContain('Runtime not installed');
    for (const label of ACTION_LABELS) expect(html).not.toContain(label);
  }
  const drainingPool = { ...connection, managedPool: { ...allocation, state: 'draining' } };
  const draining = await markup(drainingPool);
  expect(draining).toContain('Draining');
  expect(await offered(drainingPool)).toEqual([{ label: 'Enable pool', blocked: undefined, selectable: true }]);
  const unavailablePool = { ...connection, runtimeAvailable: false,
    managedPool: { ...allocation, state: 'draining' } };
  const unavailable = await markup(unavailablePool);
  expect(unavailable).toContain('0 / 2');
  // Включение без runtime открыло бы выделения, которые нечем выполнить.
  expect(await offered(unavailablePool)).toEqual([{
    label: 'Enable pool', blocked: en.proxmox_connections.runtime_not_installed, selectable: false,
  }]);
});

// Строка подключения помещается на desktop без горизонтальной прокрутки:
// пять второстепенных полей -- одна подписанная ячейка, учётные данные --
// в состоянии. Ни одно значение не теряется; строка открывает панель пула,
// последний столбец -- только стрелка без текста.
const text = (html = '') => html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
const cells = (html: string) => Object.fromEntries([...html.matchAll(
  /<td[^>]*data-label="([^"]*)"[^>]*>([\s\S]*?)<\/td>/g)].map(([, label, body]) => [label, body]));
const facts = (html = '') => [...html.matchAll(/<dt[^>]*>([\s\S]*?)<\/dt><dd[^>]*>([\s\S]*?)<\/dd>/g)]
  .map(([, term, value]) => [term, value]);
// На телефоне ячейка -- сетка «подпись | значение»: второй корневой узел
// уехал бы под подпись столбца, поэтому у значения ячейки один корень.
const roots = (html: string) => {
  let depth = 0;
  let count = 0;
  for (const [, close] of html.matchAll(/<(\/?)[a-z][^>]*>/g)) {
    if (close) depth -= 1;
    else if (depth++ === 0) count += 1;
  }
  return count;
};
const placed = { ...connection, resourcePool: 'brood-actors', storage: 'local-lvm', network: 'vmbr0' };

test('a connection row keeps four labelled columns and opens its panel without scrolling', async () => {
  const html = await markup({ ...placed, managedPool: allocation });
  const headers = [...html.matchAll(/<th[^>]*>([\s\S]*?)<\/th>/g)].map(([, label]) => text(label));
  expect(headers).toEqual(['Name', 'Connection', 'State', 'Reserved / capacity', '']);
  const row = html.slice(html.indexOf('<tbody>'));
  expect(row.match(/<td/g)).toHaveLength(headers.length);
  const opener = row.slice(row.lastIndexOf('<td'));
  expect(opener).toContain('aria-hidden="true"');
  expect(opener).toContain('<svg');
  expect(text(opener)).toBe('');
  // Строка -- одна точка фокуса, открывающая панель; кнопок в ней нет.
  expect(row.match(/tabindex="0"/g)).toHaveLength(1);
  expect(row).not.toContain('<button');
  const cell = cells(html);
  expect(Object.keys(cell)).toEqual(headers.slice(0, -1));
  expect(facts(cell.Connection)).toEqual([['Endpoint', placed.endpoint], ['Node', 'pve'],
    ['PVE resource pool', 'brood-actors'], ['Storage', 'local-lvm'], ['Network', 'vmbr0']]);
  expect(text(cell.State)).toBe('Runtime configured Enabled');
  // Число и единица не рвутся по строкам: в значении неразрывный пробел (U+00A0).
  const nb = ' ';
  expect(facts(cell['Reserved / capacity'])).toEqual([['Actors', '0 / 2'],
    ['CPU', `0${nb}CPU / 2${nb}CPU`], ['Memory', `0${nb}MiB / 8388608${nb}GiB`],
    ['Disk', `0${nb}MiB / 2${nb}GiB`]]);
  for (const body of Object.values(cell)) expect(roots(body)).toBeLessThanOrEqual(1);
});

test('an unready connection shows missing runtime and credential in its state cell', async () => {
  const html = await markup({ ...placed, runtimeAvailable: false, credentialConfigured: false });
  const cell = cells(html);
  expect(text(cell.State)).toBe(en.proxmox_connections.runtime_not_installed);
  expect(cell['Reserved / capacity']).toBe('—');
  expect(facts(cell.Connection)).toHaveLength(5);
  for (const body of Object.values(cell)) expect(roots(body)).toBeLessThanOrEqual(1);
  expect(html).not.toContain('<button');
});

test('the pool panel lists the connection and capacity in human units, never token data', async () => {
  const html = await details({ ...placed, managedPool: allocation,
    tokenSecret: 'SYNTHETIC_SECRET_SENTINEL' } as ProxmoxPool);
  const nb = '\u00a0';
  expect(facts(html)).toEqual([['Endpoint', placed.endpoint], ['Node', 'pve'],
    ['PVE resource pool', 'brood-actors'], ['Storage', 'local-lvm'], ['Network', 'vmbr0'],
    ['Actors', '0 / 2'], ['CPU', `0${nb}CPU / 2${nb}CPU`], ['Memory', `0${nb}MiB / 8388608${nb}GiB`],
    ['Disk', `0${nb}MiB / 2${nb}GiB`]]);
  for (const forbidden of ['SYNTHETIC_SECRET_SENTINEL', placed.tokenID, '<input', '<button']) {
    expect(html).not.toContain(forbidden);
  }
  const unmanaged = await details(placed);
  expect(facts(unmanaged)).toHaveLength(5);
  expect(text(unmanaged)).toContain('—');
});

test('server refuses activation visibly rather than synthesizing readiness', async () => {
  globalThis.fetch = (async () => Response.json({ detail: 'installed runtime unavailable' }, { status: 409 })) as typeof fetch;
  expect(typeof proxmoxPoolsApi.activate).toBe('function');
  await expect(proxmoxPoolsApi.activate('limited')).rejects.toMatchObject({ status: 409, message: 'installed runtime unavailable' });
});
