// FEATURE: Все действия актора — в его боковой панели (#151, #157).
// SPEC: Главное действие зависит от состояния (терминал у работающего, запуск у
// остановленного), Stop — второстепенное; остальные — группами, необратимые
// отдельным блоком. Недоступное действие несёт причину словами, а не только
// серый цвет; идущее действие актора блокирует остальные. Viewer не получает
// изменяющих действий. Необратимое требует ввода имени. Origin проверяется до
// отправки теми же правилами, что и сервер.
import { expect, test } from 'bun:test';
import { createInstance, type TFunction } from 'i18next';
import {
  confirmationOf,
  managedActionGroups,
  managedActionPlan,
  sessionReadable,
  type ActionState,
  type ManagedActionId,
  type ManagedActionPlan,
} from '@/features/substrate/actorActions';
import type { PanelActionGroup } from '@/features/substrate/components/EntityPanel';
import { originProblem } from '@/features/substrate/originDraft';
import { actorCreateDraft, nativeActionGroups } from '@/features/substrate/actorLifecycle';
import type { Actor, ActorAction, ActorTemplate } from '@/services/brood/substrate';
import en from '../src/i18n/locales/en.json';

const capabilities = {
  workspace: true,
  terminal: true,
  controlledNetwork: true,
  docker: false,
  kind: false,
  memoryCheckpoint: false,
};
const actor = (phase: Actor['phase'], desired: Actor['desired'] = 'running'): Actor => ({
  metadata: { name: 'coding', uid: 'u-1' },
  provider: 'proxmox',
  phase,
  desired,
  capabilities,
});

const flat = (plan: ManagedActionPlan) => {
  const all: ActionState[] = [plan.primary, plan.stop, ...plan.groups.flatMap((g) => g.actions)];
  return Object.fromEntries(all.map((a) => [a.id, a.enabled ? 'on' : a.reason]));
};

test('running actor: terminal is primary, stop secondary, every menu action enabled', () => {
  const plan = managedActionPlan(actor('running'));
  expect(plan.primary.id).toBe('terminal');
  expect(plan.stop.id).toBe('stop');
  expect(plan.groups.map((g) => [g.id, g.actions.map((a) => a.id)])).toEqual([
    ['session', ['restart']],
    ['workspace', ['recycle', 'wipe']],
    ['danger', ['reset', 'delete']],
  ]);
  expect(flat(plan)).toEqual({
    terminal: 'on',
    stop: 'on',
    restart: 'on',
    recycle: 'on',
    wipe: 'on',
    reset: 'on',
    delete: 'on',
  });
});

test('a profile without a terminal names why the primary action is unavailable', () => {
  const plan = managedActionPlan({ ...actor('running'), capabilities: { ...capabilities, terminal: false } });
  expect(plan.primary).toEqual({ id: 'terminal', enabled: false, reason: 'no_terminal' });
  expect(plan.stop.enabled).toBe(true);
});

test('stopped actor: start is primary; session commands need a running actor', () => {
  const plan = managedActionPlan(actor('stopped', 'stopped'));
  expect(plan.primary.id).toBe('start');
  expect(flat(plan)).toEqual({
    start: 'on',
    stop: 'stopped',
    restart: 'not_running',
    recycle: 'not_running',
    wipe: 'not_running',
    reset: 'on',
    delete: 'on',
  });
});

test('failed actor can be started, stopped, recreated or deleted, but not driven', () => {
  expect(flat(managedActionPlan(actor('error')))).toEqual({
    start: 'on',
    stop: 'on',
    restart: 'not_running',
    recycle: 'not_running',
    wipe: 'not_running',
    reset: 'on',
    delete: 'on',
  });
});

test('transitions block everything but delete, with the transition as the reason', () => {
  const provisioning = managedActionPlan(actor('provisioning'));
  expect(provisioning.primary).toEqual({ id: 'terminal', enabled: false, reason: 'transition' });
  expect(flat(provisioning)).toEqual({
    terminal: 'transition',
    stop: 'transition',
    restart: 'transition',
    recycle: 'transition',
    wipe: 'transition',
    reset: 'transition',
    delete: 'on',
  });
  const stopping = managedActionPlan(actor('stopping', 'stopped'));
  expect(stopping.primary).toEqual({ id: 'start', enabled: false, reason: 'transition' });
  // Stop requested while the phase still says running: not a live session.
  expect(flat(managedActionPlan(actor('running', 'stopped'))).restart).toBe('transition');
  expect(flat(managedActionPlan({ ...actor('running'), phase: undefined })).stop).toBe(
    'transition'
  );
});

test('requested deletion leaves only the idempotent delete retry', () => {
  for (const value of [actor('deleting', 'deleted'), actor('error', 'deleted')]) {
    const states = flat(managedActionPlan(value));
    expect(states.delete).toBe('on');
    for (const id of ['start', 'stop', 'restart', 'recycle', 'wipe', 'reset']) {
      expect(states[id]).toBe('deleting');
    }
  }
});

test('an action in flight blocks the whole row against double submits', () => {
  const states = flat(managedActionPlan(actor('running'), 'restart'));
  expect(new Set(Object.values(states))).toEqual(new Set(['pending']));
});

async function english() {
  const instance = createInstance();
  await instance.init({ lng: 'en', resources: { en: { translation: en } } });
  return instance.t;
}

// Группа панели одной строкой на действие: id, тон и что с ним можно сделать.
const shape = (groups: PanelActionGroup[]) =>
  groups.map((group) => ({
    id: group.id,
    label: group.label,
    danger: Boolean(group.danger),
    actions: group.actions.map((a) => [
      a.id,
      a.tone ?? '',
      a.pending ? 'pending' : a.blocked ? a.blocked : a.href ? `link ${a.href}` : 'on',
    ]),
  }));

const TERMINAL = '/api/swarms/swarm/proxy/actors/coding/';

async function managedGroups(value: Actor, pending?: ManagedActionId, canManage = true) {
  const calls: ManagedActionId[] = [];
  const groups = managedActionGroups(value, {
    t: await english(),
    canManage,
    pending,
    terminal: TERMINAL,
    onAction: (id) => calls.push(id),
  });
  return { groups, calls };
}

test('running actor panel: terminal link is primary, then stop, session, working copy, irreversible last', async () => {
  const { groups, calls } = await managedGroups(actor('running'));
  expect(shape(groups)).toEqual([
    {
      id: 'main',
      label: 'Actions',
      danger: false,
      actions: [
        ['terminal', 'primary', `link ${TERMINAL}`],
        ['stop', '', 'on'],
      ],
    },
    { id: 'session', label: 'Session', danger: false, actions: [['restart', '', 'on']] },
    {
      id: 'workspace',
      label: 'Working copy',
      danger: false,
      actions: [
        ['recycle', '', 'on'],
        ['wipe', '', 'on'],
      ],
    },
    {
      id: 'danger',
      label: 'Irreversible',
      danger: true,
      actions: [
        ['reset', 'danger', 'on'],
        ['delete', 'danger', 'on'],
      ],
    },
  ]);
  // Each action says what it does; the terminal is a link, never a mutation.
  const all = groups.flatMap((group) => group.actions);
  expect(all.find((a) => a.id === 'restart')?.description).toBe(en.actors.ops.restart.hint);
  expect(all.find((a) => a.id === 'terminal')?.onSelect).toBeUndefined();
  for (const action of all) action.onSelect?.();
  expect(calls).toEqual(['stop', 'restart', 'recycle', 'wipe', 'reset', 'delete']);
});

test('stopped actor panel: start is primary, unavailable actions name their reason', async () => {
  const { groups } = await managedGroups(actor('stopped', 'stopped'));
  const notRunning = en.actors.ops.reasons.not_running;
  expect(shape(groups).map((group) => group.actions)).toEqual([
    [
      ['start', 'primary', 'on'],
      ['stop', '', en.actors.ops.reasons.stopped],
    ],
    [['restart', '', notRunning]],
    [
      ['recycle', '', notRunning],
      ['wipe', '', notRunning],
    ],
    [
      ['reset', 'danger', 'on'],
      ['delete', 'danger', 'on'],
    ],
  ]);
  expect(groups.flatMap((group) => group.actions).some((a) => a.href)).toBe(false);
});

test('a transition or a profile without a terminal blocks the terminal link with a reason', async () => {
  const provisioning = (await managedGroups(actor('provisioning'))).groups;
  const terminal = provisioning[0].actions[0];
  expect(terminal).toMatchObject({ id: 'terminal', tone: 'primary' });
  expect(terminal.href).toBeUndefined();
  expect(terminal.blocked).toBe('Unavailable while the actor is provisioning. Wait for it to finish.');
  // Delete stays available: the server takes a repeated delete as its completion.
  expect(provisioning.at(-1)?.actions.at(-1)).toMatchObject({ id: 'delete' });
  expect(provisioning.at(-1)?.actions.at(-1)?.blocked).toBeUndefined();

  const bare = { ...actor('running'), capabilities: { ...capabilities, terminal: false } };
  const noTerminal = (await managedGroups(bare)).groups[0].actions[0];
  expect(noTerminal.blocked).toBe(en.actors.ops.reasons.no_terminal);
  expect(noTerminal.href).toBeUndefined();
});

test('an action in flight is marked pending and locks every other action of the actor', async () => {
  const { groups, calls } = await managedGroups(actor('running'), 'restart');
  const all = groups.flatMap((group) => group.actions);
  expect(all.filter((a) => a.pending).map((a) => a.id)).toEqual(['restart']);
  for (const action of all.filter((a) => a.id !== 'restart')) {
    expect(action.blocked, action.id).toBe(en.actors.ops.reasons.pending);
    expect(action.href, action.id).toBeUndefined();
  }
  expect(all.find((a) => a.id === 'restart')?.blocked).toBeUndefined();
  expect(calls).toEqual([]);
});

test('viewers get no managed actions in any state', async () => {
  for (const phase of ['provisioning', 'running', 'stopping', 'stopped', 'deleting', 'error'] as const) {
    expect((await managedGroups(actor(phase), undefined, false)).groups).toEqual([]);
  }
});

// Ключ вместо перевода: группы native-актора проверяются по тому, что они берут.
const keys = ((key: string) => key) as unknown as TFunction;
const native = (state: string): Actor => ({
  metadata: { name: 'luna' },
  status: { state: `ACTOR_STATE_${state}` },
});

function nativeGroups(value: Actor, pending?: ActorAction, canManage = true) {
  const calls: string[] = [];
  const groups = nativeActionGroups(value, {
    t: keys,
    canManage,
    pending,
    terminal: '/terminal/luna/',
    onAction: (action) => calls.push(action),
    onRemove: () => calls.push('remove'),
  });
  return { groups, calls };
}

test('native panel keeps the former row actions: terminal and suspend, delete apart', () => {
  const { groups, calls } = nativeGroups(native('RUNNING'));
  expect(shape(groups)).toEqual([
    {
      id: 'main',
      label: 'substrate.actions',
      danger: false,
      actions: [
        ['terminal', 'primary', 'link /terminal/luna/'],
        ['suspend', '', 'on'],
      ],
    },
    {
      id: 'danger',
      label: 'substrate.panel_danger',
      danger: true,
      actions: [['delete', 'danger', 'on']],
    },
  ]);
  expect(groups.flatMap((g) => g.actions).map((a) => [a.label, a.description])).toEqual([
    ['actors.ops.terminal.label', 'actors.native.terminal_hint'],
    ['actors.suspend', 'actors.native.suspend_hint'],
    ['substrate.delete', 'actors.native.delete_hint'],
  ]);
  for (const action of groups.flatMap((g) => g.actions)) action.onSelect?.();
  expect(calls).toEqual(['suspend', 'remove']);
});

test('a suspended native actor leads with resume; other states keep only the terminal', () => {
  expect(shape(nativeGroups(native('SUSPENDED')).groups)[0].actions).toEqual([
    ['resume', 'primary', 'on'],
    ['terminal', '', 'link /terminal/luna/'],
  ]);
  for (const state of ['RESUMING', 'CRASHED', 'UNSPECIFIED']) {
    expect(shape(nativeGroups(native(state)).groups)[0].actions).toEqual([
      ['terminal', 'primary', 'link /terminal/luna/'],
    ]);
  }
});

test('a native action in flight waits for its outcome; delete stays as before', () => {
  const { groups } = nativeGroups(native('RUNNING'), 'suspend');
  expect(shape(groups).map((group) => group.actions)).toEqual([
    [
      ['terminal', 'primary', 'link /terminal/luna/'],
      ['suspend', '', 'pending'],
    ],
    [['delete', 'danger', 'on']],
  ]);
});

test('viewers get no native actions, and managed actors never take the native groups', () => {
  expect(nativeGroups(native('RUNNING'), undefined, false).groups).toEqual([]);
  expect(nativeGroups(actor('running')).groups).toEqual([]);
});

test('confirmation strength follows reversibility', () => {
  expect(confirmationOf('terminal')).toBe('none');
  expect(confirmationOf('start')).toBe('none');
  expect(confirmationOf('stop')).toBe('none');
  expect(confirmationOf('restart')).toBe('confirm');
  for (const id of ['recycle', 'wipe', 'reset', 'delete'] as const) {
    expect(confirmationOf(id)).toBe('typed');
  }
});

test('session data is readable only while the actor is running and meant to run', () => {
  expect(sessionReadable(actor('running'))).toBe(true);
  expect(sessionReadable(actor('running', 'stopped'))).toBe(false);
  expect(sessionReadable(actor('stopped', 'stopped'))).toBe(false);
  expect(sessionReadable(actor('error'))).toBe(false);
  expect(sessionReadable({ metadata: { name: 'native' } })).toBe(false);
});

test('origin drafts follow the server canonical rules before submit', () => {
  expect(originProblem('')).toBeNull();
  expect(originProblem('https://git.example.com/team/repo.git')).toBeNull();
  expect(originProblem('https://git.example.com:443/team/repo')).toBeNull();
  expect(originProblem('  https://git.example.com/team/repo  ')).toBeNull();
  expect(originProblem('http://git.example.com/team/repo')).toBe('scheme');
  expect(originProblem('git@git.example.com:team/repo.git')).toBe('scheme');
  expect(originProblem('https://user:token@git.example.com/repo')).toBe('credentials');
  expect(originProblem('https://user@git.example.com/repo')).toBe('credentials');
  expect(originProblem('https://git.example.com/repo?ref=main')).toBe('query');
  expect(originProblem('https://git.example.com/repo#main')).toBe('query');
  expect(originProblem('https://git.example.com:8443/repo')).toBe('port');
  expect(originProblem('https://10.0.0.1/repo')).toBe('host');
  expect(originProblem('https://[::1]/repo')).toBe('host');
  expect(originProblem('https://git.example.com')).toBe('path');
  expect(originProblem('https://git.example.com/')).toBe('path');
  expect(originProblem('https://git.example.com/re\u0007po')).toBe('control');
  expect(originProblem(`https://git.example.com/${'a'.repeat(1024)}`)).toBe('length');
  expect(originProblem('https://')).toBe('invalid');
});

test('managed create carries an explicit origin only when one is given', () => {
  const template: ActorTemplate = {
    metadata: { name: 'claude-proxmox-pool' },
    provider: 'proxmox',
  };
  expect(actorCreateDraft('swarm', 'coding', template, {}, '', ' https://git.example.com/a/b ')).toEqual({
    metadata: { name: 'coding' },
    actorTemplate: { name: 'claude-proxmox-pool' },
    origin: 'https://git.example.com/a/b',
  });
  expect(actorCreateDraft('swarm', 'coding', template, {}, '', '')).toEqual({
    metadata: { name: 'coding' },
    actorTemplate: { name: 'claude-proxmox-pool' },
  });
  // Native protojson has no origin field: ateapi would refuse the unknown key.
  expect(
    actorCreateDraft('swarm', 'n', { metadata: { name: 'native' } }, {}, '', 'https://h.example/a')
  ).not.toHaveProperty('origin');
});
