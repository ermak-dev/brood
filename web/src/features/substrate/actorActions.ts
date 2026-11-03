import type { TFunction } from 'i18next';
import type { Actor } from '@/services/brood/substrate';

/** Действия managed-актора. terminal — ссылка, остальные — изменения. */
export type ManagedActionId =
  | 'terminal'
  | 'start'
  | 'stop'
  | 'restart'
  | 'recycle'
  | 'wipe'
  | 'reset'
  | 'delete';

/** Почему действие сейчас недоступно; панель показывает причину словами. */
export type BlockReason =
  | 'transition'
  | 'deleting'
  | 'not_running'
  | 'stopped'
  | 'no_terminal'
  | 'pending';

export interface ActionState {
  id: ManagedActionId;
  enabled: boolean;
  reason?: BlockReason;
}

export type ActionGroupId = 'session' | 'workspace' | 'danger';

export interface ManagedActionPlan {
  primary: ActionState;
  stop: ActionState;
  groups: { id: ActionGroupId; actions: ActionState[] }[];
}

const GROUPS: { id: ActionGroupId; actions: ManagedActionId[] }[] = [
  { id: 'session', actions: ['restart'] },
  { id: 'workspace', actions: ['recycle', 'wipe'] },
  { id: 'danger', actions: ['reset', 'delete'] },
];

const SESSION: ReadonlySet<ManagedActionId> = new Set(['restart', 'recycle', 'wipe']);

/** Работающий CT, который и должен работать: только тогда сервер пускает
 * команды сессии (relay подключён, desired=running). */
export function sessionReadable(actor: Actor): boolean {
  return (
    actor.provider === 'proxmox' &&
    actor.phase === 'running' &&
    (actor.desired ?? 'running') === 'running'
  );
}

// Причина блокировки одного действия в текущем состоянии; undefined — можно.
// Delete доступен всегда: сервер принимает повтор удаления как его завершение.
function blockOf(actor: Actor, id: ManagedActionId): BlockReason | undefined {
  if (id === 'delete') return undefined;
  const phase = actor.phase ?? '';
  if (actor.desired === 'deleted' || phase === 'deleting') return 'deleting';
  if (sessionReadable(actor)) {
    if (id === 'terminal' && !actor.capabilities?.terminal) return 'no_terminal';
    return undefined;
  }
  // Ошибка — устоявшееся состояние при любом desired: сервер принимает и
  // повторный stop, и start. Stopped с desired=running — запуск уже принят.
  const settled = phase === 'error' || (phase === 'stopped' && actor.desired !== 'running');
  if (!settled) return 'transition';
  if (SESSION.has(id) || id === 'terminal') return 'not_running';
  if (id === 'stop' && phase === 'stopped') return 'stopped';
  return undefined;
}

function stateOf(actor: Actor, id: ManagedActionId, pending: boolean): ActionState {
  if (pending) return { id, enabled: false, reason: 'pending' };
  const reason = blockOf(actor, id);
  return reason ? { id, enabled: false, reason } : { id, enabled: true };
}

/** Что показать в строке managed-актора: главное действие по состоянию
 * (терминал у работающего или запускаемого, Start у остальных), Stop рядом,
 * остальное — меню по группам, опасные последними. Пока действие строки
 * выполняется, вся строка заблокирована: второй клик не уходит на сервер. */
export function managedActionPlan(
  actor: Actor,
  pending?: ManagedActionId | null
): ManagedActionPlan {
  const busy = Boolean(pending);
  const live = actor.phase === 'running' || actor.phase === 'provisioning';
  const primary: ManagedActionId = live && actor.desired !== 'stopped' ? 'terminal' : 'start';
  return {
    primary: stateOf(actor, primary, busy),
    stop: stateOf(actor, 'stop', busy),
    groups: GROUPS.map((group) => ({
      id: group.id,
      actions: group.actions.map((id) => stateOf(actor, id, busy)),
    })),
  };
}

export type Confirmation = 'none' | 'confirm' | 'typed';

/** Обратимое — без вопроса; restart теряет только живую сессию — простое
 * подтверждение; потеря файлов или CT — ввод имени актора. */
export function confirmationOf(id: ManagedActionId): Confirmation {
  if (id === 'restart') return 'confirm';
  if (id === 'recycle' || id === 'wipe' || id === 'reset' || id === 'delete') return 'typed';
  return 'none';
}

/** Причина недоступности словами; переходное состояние называется по имени. */
export function blockedText(actor: Actor, state: ActionState, t: TFunction): string | undefined {
  if (state.enabled || !state.reason) return undefined;
  const phase = actor.phase ? t(`actors.managed_state.${actor.phase}`) : t('actors.ops.unknown_state');
  return t(`actors.ops.reasons.${state.reason}`, { state: phase });
}
