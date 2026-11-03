// FEATURE: Действия пулов живут в боковой панели (#157), а не в строке таблицы.
// SPEC: WorkerPool -- правка главным действием, удаление необратимо и только с
// uid/resourceVersion. Подключение Proxmox -- одно действие по состоянию:
// активация и включение требуют установленного runtime, вывод доступен всегда
// и обратим. Каждое изменение идёт через подтверждение; без него запрос не уходит.
import { describe, expect, test } from 'bun:test';
import type { TFunction } from 'i18next';
import type { ManagedProxmoxPool, ProxmoxPool, WorkerPool } from '@/services/brood/substrate';
import type { PanelAction, PanelActionGroup } from '@/features/substrate/components/EntityPanel';
import {
  proxmoxPoolAction,
  proxmoxPoolActionGroups,
  workerPoolActionGroups,
  type Confirmation,
  type ProxmoxPoolActionId,
} from '@/features/substrate/poolActions';

const t = ((key: string, options?: { name?: string }) =>
  options?.name ? `${key}(${options.name})` : key) as unknown as TFunction;

const shape = (groups: PanelActionGroup[]) =>
  groups.map((group) => ({ id: group.id, danger: Boolean(group.danger), actions: group.actions.map((a) => a.id) }));

const only = (groups: PanelActionGroup[]): PanelAction => {
  expect(groups).toHaveLength(1);
  expect(groups[0].actions).toHaveLength(1);
  return groups[0].actions[0];
};

describe('WorkerPool panel', () => {
  const pool: WorkerPool = {
    metadata: { name: 'dev', uid: 'u1', resourceVersion: '7' },
    spec: { replicas: 2, workerImage: 'img', sandboxClasses: [{ name: 'gvisor' }] },
  };
  const build = (target: WorkerPool) => {
    const asked: Confirmation[] = [];
    const calls: string[] = [];
    const groups = workerPoolActionGroups(target, {
      t,
      confirm: (confirmation) => asked.push(confirmation),
      edit: () => calls.push('edit'),
      remove: async () => calls.push('remove'),
    });
    return { groups, asked, calls };
  };

  test('edit is the primary action, delete sits alone in the irreversible group', () => {
    const { groups } = build(pool);
    expect(shape(groups)).toEqual([
      { id: 'main', danger: false, actions: ['edit'] },
      { id: 'danger', danger: true, actions: ['delete'] },
    ]);
    expect(groups[0].actions[0].tone).toBe('primary');
    expect(groups[1].actions[0].tone).toBe('danger');
    expect(groups[1].label).toBe('substrate.panel_danger');
  });

  test('edit opens the editor directly; delete only asks, and removes after confirmation', async () => {
    const { groups, asked, calls } = build(pool);
    groups[0].actions[0].onSelect!();
    expect(calls).toEqual(['edit']);
    expect(asked).toEqual([]);
    groups[1].actions[0].onSelect!();
    expect(calls).toEqual(['edit']);
    expect(asked).toHaveLength(1);
    expect(asked[0]).toMatchObject({
      title: 'worker_pools.delete_title',
      message: 'worker_pools.delete_message(dev)',
      confirmText: 'substrate.delete',
      success: 'worker_pools.deleted(dev)',
    });
    await asked[0].run();
    expect(calls).toEqual(['edit', 'remove']);
  });

  test('a pool read without uid or resourceVersion cannot be deleted, and says why', () => {
    for (const metadata of [{ name: 'dev' }, { name: 'dev', uid: 'u1' }, { name: 'dev', resourceVersion: '7' }]) {
      const { groups, asked } = build({ ...pool, metadata });
      const remove = groups[1].actions[0];
      expect(remove.blocked).toBe('worker_pools.delete_unversioned');
      expect(remove.onSelect).toBeUndefined();
      expect(asked).toEqual([]);
      // Правка остаётся: редактор перечитывает пул и сам называет отказ.
      expect(groups[0].actions[0].blocked).toBeUndefined();
    }
  });
});

describe('Proxmox pool panel', () => {
  const connection: ProxmoxPool = {
    name: 'limited', endpoint: 'https://pve.example.invalid:8006', tokenID: 'synthetic@pve!test',
    node: 'pve', resourcePool: 'test', storage: 'test', network: 'isolated',
    credentialConfigured: true, runtimeAvailable: true,
  };
  const allocation: ManagedProxmoxPool = {
    uid: 'pool-uid', state: 'enabled', version: '1', maxActors: '2', usedActors: '0',
    cpuMillicores: '2000', usedCPUMillicores: '0', memoryBytes: '9007199254740993',
    usedMemoryBytes: '0', diskBytes: '2147483648', usedDiskBytes: '0',
  };
  const managed = (state: string, runtimeAvailable = true): ProxmoxPool =>
    ({ ...connection, runtimeAvailable, managedPool: { ...allocation, state } });
  const build = (pool: ProxmoxPool, pending = false) => {
    const asked: Confirmation[] = [];
    const performed: ProxmoxPoolActionId[] = [];
    const groups = proxmoxPoolActionGroups(pool, {
      t, pending,
      confirm: (confirmation) => asked.push(confirmation),
      perform: async (action) => performed.push(action),
    });
    return { groups, asked, performed };
  };

  test('the offered action follows the allocation state', () => {
    expect(proxmoxPoolAction(connection)).toEqual({ id: 'activate', needsRuntime: false });
    expect(proxmoxPoolAction({ ...connection, runtimeAvailable: false })).toEqual({ id: 'activate', needsRuntime: true });
    expect(proxmoxPoolAction(managed('enabled'))).toEqual({ id: 'drain', needsRuntime: false });
    expect(proxmoxPoolAction(managed('enabled', false))).toEqual({ id: 'drain', needsRuntime: false });
    expect(proxmoxPoolAction(managed('draining'))).toEqual({ id: 'enable', needsRuntime: false });
    expect(proxmoxPoolAction(managed('draining', false))).toEqual({ id: 'enable', needsRuntime: true });
  });

  test('an unknown allocation state offers nothing rather than guessing', () => {
    for (const state of ['', 'deleting', 'unknown']) {
      expect(proxmoxPoolAction(managed(state))).toBeNull();
      expect(build(managed(state)).groups).toEqual([]);
    }
  });

  test('activation and enabling are primary; draining is reversible, so it is never in the danger block', () => {
    const activate = only(build(connection).groups);
    expect([activate.id, activate.tone, activate.label]).toEqual(['activate', 'primary', 'proxmox_connections.activate']);
    expect(activate.description).toBe('proxmox_connections.activate_hint');
    const enable = only(build(managed('draining')).groups);
    expect([enable.id, enable.tone]).toEqual(['enable', 'primary']);
    const drained = build(managed('enabled'));
    const drain = only(drained.groups);
    expect([drain.id, drain.tone]).toEqual(['drain', undefined]);
    expect(drained.groups[0].danger).toBeUndefined();
    expect(drained.groups[0].label).toBe('substrate.actions');
  });

  test('without installed runtime activation and enabling are blocked with the plain reason', () => {
    for (const pool of [{ ...connection, runtimeAvailable: false }, managed('draining', false)]) {
      const { groups, asked, performed } = build(pool);
      const action = only(groups);
      expect(action.blocked).toBe('proxmox_connections.runtime_not_installed');
      expect(action.onSelect).toBeUndefined();
      expect(asked).toEqual([]);
      expect(performed).toEqual([]);
    }
    // Вывод только закрывает выделения: runtime ему не нужен.
    expect(only(build(managed('enabled', false)).groups).blocked).toBeUndefined();
  });

  test('every action asks first and calls the server only after confirmation', async () => {
    for (const [pool, id] of [
      [connection, 'activate'], [managed('enabled'), 'drain'], [managed('draining'), 'enable'],
    ] as const) {
      const { groups, asked, performed } = build(pool);
      only(groups).onSelect!();
      expect(performed).toEqual([]);
      expect(asked).toHaveLength(1);
      expect(asked[0]).toMatchObject({
        title: `proxmox_connections.${id}`,
        message: `proxmox_connections.${id}_message(limited)`,
        confirmText: `proxmox_connections.${id}`,
        success: 'proxmox_connections.action_done(limited)',
      });
      await asked[0].run();
      expect(performed).toEqual([id]);
    }
  });

  test('a running pool action marks the panel action pending', () => {
    expect(only(build(managed('enabled'), true).groups).pending).toBe(true);
    expect(only(build(managed('enabled'), false).groups).pending).toBe(false);
  });
});
