// FEATURE: Пульт managed-актора и repository проекта в панели (#151, #157).
// SPEC: Подтверждение называет последствия; необратимое требует ввода имени.
// Обзор и действия — отдельные вкладки (#159), необратимые последними.
// Недоступное действие несёт причину в accessible
// description; идущее — статус и блокировка остальных; неизвестный исход
// виден до закрытия. Viewer видит только обзор, без вкладок сессии и действий.
// Детали показывают origin, режим клона, поколение и роль. Repository
// выбирается из remotes без токенов; исчезнувший из настройки выбор помечен.
// Новые строки есть во всех локалях с теми же подстановками.
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { actionDialogText } from '@/features/substrate/actorLifecycle';
import { ActorDetailsSheet } from '@/features/substrate/ActorDetailsSheet';
import { ConfirmDialog } from '@/features/substrate/components/ConfirmDialog';
import { repositoryOptions } from '@/features/access/state';
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
const actor: Actor = {
  metadata: { name: 'coding', uid: 'u-1' },
  provider: 'proxmox',
  profile: 'claude-proxmox',
  role: 'worker',
  generation: '7',
  phase: 'running',
  desired: 'running',
  origin: 'https://git.example.com/team/repo',
  originMode: 'gateway',
  placement: { node: 'pve-a', ctid: 105, pool: 'limited' },
  capabilities,
};

// Как в src/i18n/index.ts: экранирует React, а не i18next.
async function i18n() {
  const instance = createInstance();
  await instance.init({
    lng: 'en',
    resources: { en: { translation: en } },
    interpolation: { escapeValue: false },
  });
  return instance;
}

const submitDisabled = (html: string) =>
  [...html.matchAll(/<button\b[^>]*>/g)].some(
    (m) => m[0].includes('type="submit"') && m[0].includes('disabled=""')
  );

const render = async (element: Parameters<typeof createElement>[0], props: object) => {
  const instance = await i18n();
  return renderToStaticMarkup(
    createElement(I18nextProvider, { i18n: instance }, createElement(element, props))
  );
};

test('dialogs state the consequence of each action, reset says where the workspace comes from', async () => {
  const { t } = await i18n();
  const reset = actionDialogText(actor, 'reset', t);
  expect(reset.message).toContain('permanently');
  expect(reset.message).toContain('fresh container');
  expect(reset.note).toBe('The new workspace is cloned again from https://git.example.com/team/repo.');
  expect(actionDialogText({ ...actor, origin: '' }, 'reset', t).note).toContain('starts empty');
  expect(reset.confirmText).toBe('Recreate');
  const recycle = actionDialogText(actor, 'recycle', t);
  expect(recycle.title).toBe('Clean the working copy of coding?');
  expect(recycle.message).toContain('untracked and ignored');
  expect(recycle.message).toContain('Commits');
  expect(recycle.message).toContain('cannot be recovered');
  expect(actionDialogText(actor, 'wipe', t).message).toContain('refuses while a Claude session is running');
  expect(actionDialogText(actor, 'restart', t).message).toContain('are kept');
  expect(actionDialogText(actor, 'delete', t).message).toContain('cannot be undone');
});

test('an irreversible confirmation stays disabled until the exact name is typed', async () => {
  const typed = await render(ConfirmDialog, {
    open: true,
    title: 'Delete',
    confirmText: 'Delete',
    typedName: 'coding',
    onConfirm: () => {},
    onCancel: () => {},
    children: createElement('p', null, 'Gone for good'),
  });
  expect(typed).toContain('Type coding to confirm');
  expect(submitDisabled(typed)).toBe(true);
  expect(typed.toLowerCase()).toContain('autocomplete="off"');
  const plain = await render(ConfirmDialog, {
    open: true,
    title: 'Restart',
    confirmText: 'Restart Claude',
    onConfirm: () => {},
    onCancel: () => {},
    children: createElement('p', null, 'Session ends'),
  });
  expect(plain).not.toContain('to confirm');
  expect(submitDisabled(plain)).toBe(false);
});

const details = {
  open: true,
  onClose: () => {},
  actor,
  swarm: 'swarm',
  swarmName: 'team',
  administrator: false,
  failure: null,
  unknown: [],
  onAction: () => {},
  onClearFailure: () => {},
  onDismissUnknown: () => {},
};

const strip = (html: string) => html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
// Кнопка или ссылка панели по видимому названию (первый текст внутри).
const control = (html: string, label: string) =>
  [...html.matchAll(/<(button|a)\b([^>]*)>([\s\S]*?)<\/\1>/g)]
    .map(([, tag, attrs, inner]) => ({ tag, attrs, inner, text: strip(inner) }))
    .find((item) => item.text === label || item.text.startsWith(`${label} `));
const headings = (html: string) => [...html.matchAll(/<h3[^>]*>([^<]+)<\/h3>/g)].map((m) => m[1]);

test('a blocked panel action stays focusable and describes its reason', async () => {
  const html = await render(ActorDetailsSheet, {
    ...details,
    canManage: true,
    actor: { ...actor, phase: 'stopped', desired: 'stopped' },
  });
  const stop = control(html, 'Stop');
  expect(stop?.tag).toBe('button');
  expect(stop?.attrs).toContain('aria-disabled="true"');
  expect(stop?.attrs).not.toContain('disabled=""');
  const described = stop?.attrs.match(/aria-describedby="([^"]+)"/)?.[1];
  const description = stop?.inner.match(new RegExp(`id="${described}"[^>]*>([\\s\\S]*)</span>`))?.[1];
  expect(strip(description ?? '')).toContain('The actor is already stopped.');
  // An available action describes its effect instead and can be pressed.
  const start = control(html, 'Start');
  expect(start?.attrs).not.toContain('aria-disabled');
  expect(start?.text).toContain('Starts the container.');
});

test('an action in flight shows its status, locks the rest; unknown outcomes stay until dismissed', async () => {
  const html = await render(ActorDetailsSheet, {
    ...details,
    canManage: true,
    pending: 'restart',
    unknown: [
      { name: 'coding', action: 'wipe', detail: 'relay lost' },
      { name: 'other', action: 'stop', detail: '' },
    ],
  });
  expect(html).toMatch(/role="status"[^>]*><span class="loading-spinner"[^>]*><\/span>Restarting…<\/span>/);
  const restart = control(html, 'Restart Claude');
  expect(restart?.attrs).toContain('aria-busy="true"');
  expect(restart?.attrs).toContain('aria-disabled="true"');
  for (const label of ['Stop', 'Clean working copy', 'Wipe working copy', 'Recreate', 'Delete']) {
    const item = control(html, label);
    expect(item?.tag, label).toBe('button');
    expect(item?.attrs, label).toContain('aria-disabled="true"');
    expect(item?.text, label).toContain(en.actors.ops.reasons.pending);
  }
  expect(html).not.toContain('href=');
  expect(html).toContain('role="alert"');
  expect(html).toContain('Outcome unknown: Wipe working copy on coding');
  expect(html).toContain('relay lost');
  expect(html).not.toContain('on other');
});

test('viewers get the overview without session tabs or actions; managers get the console', async () => {
  const props = details;
  const viewer = await render(ActorDetailsSheet, { ...props, canManage: false });
  expect(viewer).not.toContain('role="tablist"');
  expect(viewer).not.toContain('href=');
  expect(headings(viewer)).toEqual([]);
  expect(viewer).not.toContain('aria-describedby');
  for (const action of ['Open terminal', 'Stop', 'Restart Claude', 'Recreate', 'Delete']) {
    expect(control(viewer, action), action).toBeUndefined();
  }
  expect(viewer).not.toContain(en.actors.console.viewer_hint);
  for (const fact of [
    'https://git.example.com/team/repo',
    en.actors.console.overview.modes.gateway,
    'Generation',
    '>7<',
    '>worker<',
    'pve-a',
  ]) {
    expect(viewer).toContain(fact);
  }

  const manager = await render(ActorDetailsSheet, { ...props, canManage: true });
  expect(manager).toContain('role="tablist"');
  const tabs = [...manager.matchAll(/role="tab"[^>]*aria-selected="(true|false)"[^>]*>([^<]+)</g)];
  expect(tabs.map((m) => [m[2], m[1]])).toEqual([
    ['Overview', 'true'],
    ['Actions', 'false'],
  ]);
  // Обзор открыт первым; действия в отдельной скрытой вкладке, терминал в списке.
  expect(headings(manager)).toEqual(['Actions', 'Session', 'Working copy', 'Irreversible']);
  const panelsStart = manager.indexOf('role="tabpanel"');
  const actionsStart = manager.indexOf('role="tabpanel"', panelsStart + 1);
  const overview = manager.slice(panelsStart, actionsStart);
  const actions = manager.slice(actionsStart);
  expect(overview).toContain('Generation');
  expect(overview).not.toContain('>Restart Claude<');
  expect(actions).toContain('>Restart Claude<');
  expect(actions).not.toContain('Generation');
  expect(actions.indexOf('>Working copy<')).toBeLessThan(actions.indexOf('>Irreversible<'));
  expect(control(manager, 'Open terminal')).toBeUndefined();
  expect(manager).not.toContain('aria-haspopup="menu"');
  // Every tab controls an existing panel; session panels mount only when opened,
  // so rendering the details reads nothing from the session.
  const panels = [...manager.matchAll(/role="tabpanel" id="([^"]+)"/g)].map((m) => m[1]);
  const controls = [...manager.matchAll(/aria-controls="([^"]+)"/g)].map((m) => m[1]);
  expect(panels).toEqual(controls);
  expect(manager.match(/role="tabpanel"[^>]*hidden=""/g)?.length).toBe(1);
  for (const absent of [en.actors.console.output.lines, en.actors.console.message.cost]) {
    expect(manager).not.toContain(absent);
  }
});

test('repository choices show credential presence and keep a vanished selection visible', () => {
  const labels = { none: 'None', credentials: 'credentials', public: 'public', missing: 'gone' };
  expect(
    repositoryOptions(
      [
        { origin: 'https://git.example.com/a', credentialConfigured: true },
        { origin: 'https://git.example.com/b', credentialConfigured: false },
      ],
      'https://old.example.com/c',
      labels
    )
  ).toEqual([
    { value: '', label: 'None' },
    { value: 'https://git.example.com/a', label: 'https://git.example.com/a · credentials' },
    { value: 'https://git.example.com/b', label: 'https://git.example.com/b · public' },
    { value: 'https://old.example.com/c', label: 'https://old.example.com/c · gone' },
  ]);
  expect(repositoryOptions([], '', labels)).toEqual([{ value: '', label: 'None' }]);
});

test('project details show the repository; only administrators edit it', () => {
  const source = (path: string) =>
    readFileSync(new URL(`../src/features/access/${path}`, import.meta.url), 'utf8');
  expect(source('ProjectDetailPage.tsx')).toContain('<ProjectRepository project={project} admin={admin} />');
  const repository = source('ProjectRepository.tsx');
  expect(repository).toContain('admin ? accessApi.gitRemotes()');
  expect(repository).toMatch(/\{admin \? \(/);
  expect(repository).toContain('accessApi.setRepository(project.id, selected)');
  expect(repository).not.toMatch(/token/i);
});

type Tree = { [key: string]: string | Tree };
const leaves = (tree: Tree, prefix = ''): [string, string][] =>
  Object.entries(tree).flatMap(([key, value]) =>
    typeof value === 'string' ? [[`${prefix}${key}`, value]] : leaves(value, `${prefix}${key}.`)
  );
const variables = (text: string) => [...text.matchAll(/\{\{(\w+)\}\}/g)].map((m) => m[1]).sort();
const plural = /_(one|few|many|other)$/;

test('every new string exists in all six locales with the same placeholders', () => {
  const pick = (messages: Record<string, Tree>) =>
    new Map(
      [
        ...leaves(messages.actors.ops as Tree, 'actors.ops.'),
        ...leaves(messages.actors.console as Tree, 'actors.console.'),
        ...leaves((messages.actors.native ?? {}) as Tree, 'actors.native.'),
        ...leaves(messages.access.repository as Tree, 'access.repository.'),
        ...['origin', 'origin_hint', 'origin_repository', 'origin_no_repository',
          'origin_repository_unavailable', 'reset', 'reset_message'].map(
          (key) => [`actors.${key}`, messages.actors[key] as string] as [string, string]
        ),
        ...leaves(messages.actors.origin_invalid as Tree, 'actors.origin_invalid.'),
      ].map(([key, value]) => [key.replace(plural, ''), value])
    );
  const reference = pick(en as unknown as Record<string, Tree>);
  expect(reference.size).toBeGreaterThan(150);
  for (const locale of ['ru', 'zh-CN', 'zh-TW', 'ko', 'vi']) {
    const messages = JSON.parse(
      readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
    );
    const strings = pick(messages);
    for (const [key, value] of reference) {
      const translated = strings.get(key);
      expect(typeof translated === 'string' && translated.length > 0, `${locale}: ${key}`).toBe(true);
      expect(variables(translated!), `${locale}: ${key}`).toEqual(variables(value));
    }
  }
  const ru = JSON.parse(readFileSync(new URL('../src/i18n/locales/ru.json', import.meta.url), 'utf8'));
  expect(Object.keys(ru.actors.console.usage).filter((k) => k.startsWith('days_')).sort()).toEqual([
    'days_few',
    'days_many',
    'days_one',
    'days_other',
  ]);
});
