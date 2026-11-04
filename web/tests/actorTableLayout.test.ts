// FEATURE: Таблица акторов: одна ячейка размещения (#108), действия — в панели (#157).
// SPEC: Provider и логический пул/CTID — не отдельные колонки, а одна ячейка
// размещения: native — узел, Proxmox — «Proxmox · узел · CT (пул)» без
// выдуманного native-воркера. В строке нет кнопок: строка (карточка на
// телефоне) открывает панель актора, там все действия. Идущее действие и
// неизвестный исход видны в ячейке состояния и при закрытой панели. Viewer
// открывает только managed-детали; golden-актор не открывается.
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import * as lifecycle from '@/features/substrate/actorLifecycle';
import * as actorsPage from '@/features/substrate/ActorsPage';
import type { Actor } from '@/services/brood/substrate';
import en from '../src/i18n/locales/en.json';

const capabilities = {
  workspace: true,
  terminal: true,
  controlledNetwork: true,
  docker: false,
  kind: false,
  memoryCheckpoint: false,
};
const running: Actor = {
  metadata: { name: 'coding', uid: 'coding', updateTime: '2026-10-31T10:00:00Z' },
  actorTemplate: { atespace: 'swarm', name: 'brood-coding' },
  status: {
    state: 'ACTOR_STATE_RUNNING',
    assignedNode: 'substrate-control-plane',
    workerAssignment: { nodeName: 'other-node' },
  },
};
const suspended: Actor = {
  metadata: { name: 'lxc', uid: 'lxc' },
  actorTemplate: { atespace: 'swarm', name: 'lxc-probe' },
  status: { state: 'ACTOR_STATE_SUSPENDED', workerAssignment: { nodeName: 'worker-b' } },
};
const managed: Actor = {
  metadata: { name: 'claude', uid: 'managed', version: '9007199254740993' },
  actorTemplate: { name: 'claude-proxmox-brood-proxmox' },
  provider: 'proxmox',
  profile: 'claude-proxmox',
  generation: '9007199254740995',
  phase: 'running',
  desired: 'running',
  placement: { node: 'hyper', ctid: 105, pool: 'brood-proxmox' },
  capabilities,
  // Чужое native-поле в managed-объекте не должно стать его размещением.
  status: { state: 'ACTOR_STATE_UNSPECIFIED', assignedNode: 'fake-native-worker' },
};

test('placement is the node for native actors and the real Proxmox placement for managed ones', () => {
  expect(typeof lifecycle.actorPlacement).toBe('function');
  const { actorPlacement } = lifecycle;
  expect(actorPlacement(running)).toBe('substrate-control-plane');
  expect(actorPlacement(suspended)).toBe('worker-b');
  expect(actorPlacement({ metadata: { name: 'pending' } })).toBe('—');
  // Перенос — только после «·» и перед пулом: «CT 105» не рвётся в узкой ячейке.
  expect(actorPlacement(managed)).toBe('Proxmox\u00a0· hyper\u00a0· CT\u00a0105 (brood-proxmox)');
  expect(actorPlacement({ ...managed, placement: { node: '', pool: 'brood-proxmox' } })).toBe(
    'Proxmox (brood-proxmox)'
  );
  expect(actorPlacement({ ...managed, placement: undefined })).toBe('Proxmox');
});

async function table(rows: Actor[], canManage: boolean, extra: object = {}) {
  expect(typeof actorsPage.ActorTable).toBe('function');
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  const markup = renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(actorsPage.ActorTable, {
        rows,
        swarm: 'swarm',
        canManage,
        goldenOf: () => undefined,
        onOpen: () => {},
        ...extra,
      })
    )
  );
  const text = (html: string) => html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
  const heads = [...markup.matchAll(/<th[^>]*>([\s\S]*?)<\/th>/g)].map((m) => text(m[1]));
  const body = markup.slice(markup.indexOf('<tbody'));
  const lines = body.split('<tr').slice(1).map((row) => {
    const cells = [...row.matchAll(/<td[^>]*>([\s\S]*?)<\/td>/g)].map((m) => m[1]);
    return { row: row.slice(0, row.indexOf('>')), cells: cells.map(text), raw: cells };
  });
  return { heads, lines, markup, body };
}

// Всё, что было кнопками строки до #157: в таблице их больше нет.
const ROW_ACTIONS = ['Open terminal', 'Suspend', 'Resume', 'Start', 'Stop', 'Details', 'More', 'Edit', 'Delete'];

test('provider and pool/CTID fold into one placement column; rows hold no actions and open the panel', async () => {
  const { heads, lines, markup, body } = await table([running, suspended, managed], true);
  // Терминал — единственный прямой переход в строке; операции остаются в панели.
  expect(heads).toEqual(['Name', 'State', 'Template', 'Placement', 'Updated', 'Open terminal']);
  for (const gone of ['Provider', 'Logical pool / CTID', '>Node<', 'fake-native-worker', '>Actions<']) {
    expect(markup).not.toContain(gone);
  }
  expect(lines.map((line) => line.cells[3])).toEqual([
    'substrate-control-plane',
    'worker-b',
    'Proxmox · hyper · CT 105 (brood-proxmox)',
  ]);
  expect(markup.match(/data-label="Placement"/g)?.length).toBe(3);
  for (const locale of ['en', 'ru', 'ko', 'vi', 'zh-CN', 'zh-TW']) {
    const messages = JSON.parse(
      readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
    );
    expect(messages.substrate.placement?.length).toBeGreaterThan(0);
  }

  expect(body).not.toMatch(/<button\b/);
  expect(body).not.toContain('aria-haspopup');
  for (const action of ROW_ACTIONS.filter((action) => action !== 'Open terminal')) expect(body).not.toContain(`>${action}<`);
  for (const line of lines) {
    expect(line.row).toContain('tabindex="0"');
    expect(line.cells).toHaveLength(6);
    expect(line.raw.at(-1)).toContain('href="/api/swarms/swarm/proxy/actors/');
    expect(line.raw.at(-1)).toContain('target="_blank"');
  }
  expect(lines.map((line) => line.cells.slice(0, 2))).toEqual([
    ['coding', 'running'],
    ['lxc', 'suspended'],
    ['claude', 'running'],
  ]);
});

test('the state cell keeps an action in flight and an unknown outcome visible with the panel closed', async () => {
  const { lines } = await table([managed], true, {
    pending: { claude: 'restart' },
    unknown: [{ name: 'claude', action: 'wipe', detail: '' }],
  });
  expect(lines[0].cells[1]).toBe('running Restarting… outcome unknown');
  expect(lines[0].raw[1]).toContain('class="loading-spinner"');
});

test('viewers open read-only managed details only; golden actors never open', async () => {
  const mixed = await table([running, managed], false);
  for (const action of ROW_ACTIONS) expect(mixed.body).not.toContain(`>${action}<`);
  expect(mixed.body).not.toMatch(/<(button|a)\b/);
  expect(mixed.lines[1].row).toContain('tabindex="0"');
  // A viewer has nothing to open for a native actor: its panel is the editor.
  for (const rows of [[running], [suspended, running]]) {
    const { lines, markup } = await table(rows, false);
    for (const line of lines) expect(line.row).not.toContain('tabindex');
    expect(markup).not.toContain('<svg');
  }
  const golden = await table([running], true, { goldenOf: () => 'brood-coding' });
  expect(golden.lines[0].row).not.toContain('tabindex');
  expect(golden.lines[0].cells[0]).toBe('golden snapshot · brood-coding');
});

test('the page builds the panels from the shared builders and keeps viewer gates', () => {
  const page = readFileSync(
    new URL('../src/features/substrate/ActorsPage.tsx', import.meta.url),
    'utf8'
  );
  // Rows open what canViewActorDetails allows; the native panel takes the
  // swarm role, so a viewer would get no lifecycle or delete there either.
  expect(page).toContain('const openable = (a: Actor) => !goldenOf(a) && canViewActorDetails(canManage, a);');
  expect(page).toMatch(/nativeActionGroups\(editorActor, \{\s*t,\s*canManage,/);
  expect(page).toMatch(/\{dialog && canManage && \(\s*<ManagedActionDialog/);
  for (const [scope, state] of [['editor', 'editor'], ['details', 'details'], ['confirmation', 'dialog']]) {
    expect(page).toContain('key={`' + scope + '-${' + state + '.session}`}');
  }
  const sheet = readFileSync(
    new URL('../src/features/substrate/ActorDetailsSheet.tsx', import.meta.url),
    'utf8'
  );
  expect(sheet).toMatch(/managedActionGroups\(actor, \{\s*t,\s*canManage,/);
});
