// FEATURE: Все действия managed-актора в панели /actors (#151).
// SPEC: Главное действие зависит от состояния (терминал у работающего, запуск у
// остановленного), Stop — второстепенное; остальные — в меню группами, опасные
// отдельно. Недоступное действие несёт причину, а не только серый цвет;
// незавершённое действие строки блокирует остальные. Необратимое требует ввода
// имени. Origin проверяется до отправки теми же правилами, что и сервер.
import { expect, test } from 'bun:test';
import {
  confirmationOf,
  managedActionPlan,
  sessionReadable,
  type ActionState,
  type ManagedActionPlan,
} from '@/features/substrate/actorActions';
import { originProblem } from '@/features/substrate/originDraft';
import { actorCreateDraft } from '@/features/substrate/actorLifecycle';
import type { Actor, ActorTemplate } from '@/services/brood/substrate';

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
