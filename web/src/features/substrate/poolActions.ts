import type { TFunction } from 'i18next';
import type { ProxmoxPool, WorkerPool } from '@/services/brood/substrate';
import type { PanelAction, PanelActionGroup } from './components/EntityPanel';
import { versioned } from './workerPoolDraft';

/** Подтверждение перед изменением: то же, что принимает useActions().confirm. */
export interface Confirmation {
  title: string;
  message: string;
  confirmText: string;
  success: string;
  run: () => Promise<unknown>;
}

type Confirm = (confirmation: Confirmation) => void;

/** Действия WorkerPool в панели: правка открывает редактор, удаление --
 * необратимо и только с предусловиями uid/resourceVersion, иначе сервер не
 * сверит, что удаляется именно прочитанный пул. */
export function workerPoolActionGroups(
  pool: WorkerPool,
  { t, confirm, edit, remove }: { t: TFunction; confirm: Confirm; edit: () => void; remove: () => Promise<unknown> }
): PanelActionGroup[] {
  const name = pool.metadata.name;
  const deletable = versioned(pool);
  return [
    {
      id: 'main',
      label: t('substrate.actions'),
      actions: [
        {
          id: 'edit',
          label: t('substrate.edit'),
          description: t('worker_pools.edit_hint'),
          tone: 'primary',
          onSelect: edit,
        },
      ],
    },
    {
      id: 'danger',
      label: t('substrate.panel_danger'),
      danger: true,
      actions: [
        {
          id: 'delete',
          label: t('substrate.delete'),
          description: t('worker_pools.delete_hint'),
          tone: 'danger',
          blocked: deletable ? undefined : t('worker_pools.delete_unversioned'),
          onSelect: deletable
            ? () =>
                confirm({
                  title: t('worker_pools.delete_title'),
                  message: t('worker_pools.delete_message', { name }),
                  confirmText: t('substrate.delete'),
                  success: t('worker_pools.deleted', { name }),
                  run: remove,
                })
            : undefined,
        },
      ],
    },
  ];
}

export type ProxmoxPoolActionId = 'activate' | 'drain' | 'enable';

/** Что предложить подключению Proxmox в его состоянии. Без резервирования --
 * активация, включённое -- вывод, выводимое -- включение обратно. Активация
 * и включение открывают новые выделения, поэтому без установленного runtime
 * они заблокированы; вывод их только закрывает и доступен всегда. Неизвестное
 * состояние не угадываем. */
export function proxmoxPoolAction(pool: ProxmoxPool): { id: ProxmoxPoolActionId; needsRuntime: boolean } | null {
  const managed = pool.managedPool;
  if (!managed) return { id: 'activate', needsRuntime: !pool.runtimeAvailable };
  if (managed.state === 'enabled') return { id: 'drain', needsRuntime: false };
  if (managed.state === 'draining') return { id: 'enable', needsRuntime: !pool.runtimeAvailable };
  return null;
}

/** Действие подключения Proxmox в панели. Вывод обратим включением, поэтому
 * он в обычной группе, а не среди необратимых. pending -- идёт любое действие
 * над пулами: второе не начинается, пока не пришёл исход первого. */
export function proxmoxPoolActionGroups(
  pool: ProxmoxPool,
  {
    t,
    confirm,
    pending,
    perform,
  }: { t: TFunction; confirm: Confirm; pending: boolean; perform: (action: ProxmoxPoolActionId) => Promise<unknown> }
): PanelActionGroup[] {
  const next = proxmoxPoolAction(pool);
  if (!next) return [];
  const { id } = next;
  const blocked = next.needsRuntime ? t('proxmox_connections.runtime_not_installed') : undefined;
  const action: PanelAction = {
    id,
    label: t(`proxmox_connections.${id}`),
    description: t(`proxmox_connections.${id}_hint`),
    tone: id === 'drain' ? undefined : 'primary',
    blocked,
    pending,
    onSelect: blocked
      ? undefined
      : () =>
          confirm({
            title: t(`proxmox_connections.${id}`),
            message: t(`proxmox_connections.${id}_message`, { name: pool.name }),
            confirmText: t(`proxmox_connections.${id}`),
            success: t('proxmox_connections.action_done', { name: pool.name }),
            run: () => perform(id),
          }),
  };
  return [{ id: 'main', label: t('substrate.actions'), actions: [action] }];
}
