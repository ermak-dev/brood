// FEATURE: Таблица акторов вмещает действия рядом с managed-размещением (#108).
// SPEC: Provider и логический пул/CTID — не отдельные колонки, а одна ячейка
// размещения: native — узел, Proxmox — «Proxmox · узел · CT (пул)» без
// выдуманного native-воркера. Managed-строка: главное действие по состоянию и
// Stop, затем детали и меню остальных (#151); группы переносятся, native-строка
// — одной линией, как прежде.
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

async function table(rows: Actor[], canManage: boolean) {
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
        acting: false,
        goldenOf: () => undefined,
        onOpen: () => {},
        onAction: () => {},
        onRemove: () => {},
      })
    )
  );
  // Скрытые описания (aria-describedby) — не видимый текст строки.
  const visible = (html: string) => html.replace(/<span[^>]*hidden=""[^>]*>[\s\S]*?<\/span>/g, '');
  const text = (html: string) =>
    visible(html).replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
  const heads = [...markup.matchAll(/<th[^>]*>([\s\S]*?)<\/th>/g)].map((m) => text(m[1]));
  const body = markup.slice(markup.indexOf('<tbody'));
  const lines = body.split('<tr').slice(1).map((row) => {
    const cells = [...row.matchAll(/<td[^>]*>([\s\S]*?)<\/td>/g)].map((m) => m[1]);
    return { cells: cells.map(text), actions: visible(cells.at(-1) ?? '') };
  });
  return { heads, lines, markup, text };
}

test('provider and pool/CTID fold into one placement column, all actions stay in each row', async () => {
  const { heads, lines, markup, text } = await table([running, suspended, managed], true);
  expect(heads).toEqual(['Name', 'State', 'Template', 'Placement', 'Updated', 'Actions']);
  for (const gone of ['Provider', 'Logical pool / CTID', '>Node<', 'fake-native-worker']) {
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

  const actions = lines.map((line) => line.cells.at(-1));
  expect(actions).toEqual([
    'Open terminal Suspend Edit Delete',
    'Open terminal Resume Edit Delete',
    'Open terminal Stop Details More',
  ]);
  // Lifecycle и действия над объектом — две группы; Button несёт свой <span>,
  // поэтому кнопки и ссылки сначала сводятся к тексту.
  const control = /<(button|a)\b[^>]*>([\s\S]*?)<\/\1>/g;
  const flatten = (html: string) =>
    html.replace(control, (_, _tag, inner: string) => ` ${text(inner)} `);
  const groups = (html: string) =>
    [...flatten(html).matchAll(/<span[^>]*>([\s\S]*?)<\/span>/g)].map((m) =>
      text(m[1])
    );
  expect(groups(lines[2].actions)).toEqual(['Open terminal Stop', 'Details More']);
  // The rarer and destructive managed actions live in the menu, not in the row.
  for (const hidden of ['Recreate', 'Restart Claude', 'Clean working copy', 'Wipe']) {
    expect(lines[2].actions).not.toContain(hidden);
  }
  expect(lines[2].actions).toContain('aria-haspopup="menu"');
  expect(lines[2].actions).toContain('aria-label="More actions for claude"');
  expect(groups(lines[0].actions)).toEqual(['Open terminal Suspend', 'Edit Delete']);
});

test('viewers keep read-only managed details and see no native workload actions', async () => {
  const { lines, markup } = await table([running, managed], false);
  expect(lines.map((line) => line.cells.at(-1))).toEqual(['', 'Details']);
  for (const hidden of ['Delete', 'Suspend', 'Stop', 'Open terminal', 'Edit']) {
    expect(markup).not.toContain(hidden);
  }
});

test('the actions column takes the width its actions need and only managed groups wrap', () => {
  const page = readFileSync(
    new URL('../src/features/substrate/ActorsPage.tsx', import.meta.url),
    'utf8'
  );
  const styles = readFileSync(
    new URL('../src/features/substrate/substrate.module.scss', import.meta.url),
    'utf8'
  );
  expect(page).toContain('<div className={styles.actorTable}>');
  expect(page).toContain(
    "a.provider === 'proxmox' ? styles.actorActionsWrap : styles.actorActions"
  );
  expect(styles).toMatch(/\.actorTable \.cellActions\s*\{[^}]*width: auto;/);
  expect(styles).toMatch(/\.actorActions\s*\{[^}]*flex-wrap: nowrap;/);
  expect(styles).toMatch(/\.actorActionsWrap\s*\{[^}]*flex-wrap: wrap;/);
  expect(styles).toMatch(/\.actorActionGroup\s*\{[^}]*white-space: nowrap;/);
});
