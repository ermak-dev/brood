import type { TFunction } from 'i18next';
import type { Actor, ActorAction, ActorTemplate } from '@/services/brood/substrate';
import type { PanelAction, PanelActionGroup } from './components/EntityPanel';
import { DASH, shortState } from './format';

export function actorState(actor: Actor): string {
  return actor.provider === 'proxmox'
    ? (actor.phase ?? '')
    : shortState(actor.status?.state, 'ACTOR_STATE_');
}

// Одна ячейка вместо колонок провайдера и пула/CTID: managed-строка называет
// реальный CT Proxmox, а не native-воркер; неизвестные части не показываются.
// Неразрывные пробелы держат «CT 105» целым и не начинают строку с «·»:
// на 1280px колонка узкая и текст переносится.
export function actorPlacement(actor: Actor): string {
  if (actor.provider !== 'proxmox') {
    return actor.status?.assignedNode || actor.status?.workerAssignment?.nodeName || DASH;
  }
  const { node, ctid, pool } = actor.placement ?? {};
  const where = ['Proxmox', node, ctid == null ? '' : `CT\u00a0${ctid}`]
    .filter(Boolean)
    .join('\u00a0· ');
  return pool ? `${where} (${pool})` : where;
}

export function canViewActorDetails(canManage: boolean, actor: Actor | null): boolean {
  return canManage || actor?.provider === 'proxmox';
}

export function deleteConfirmation(actor: Actor, t: TFunction, run: () => Promise<unknown>) {
  return {
    title: t('actors.delete_title'),
    message: t(actor.provider === 'proxmox' ? 'actors.managed_delete_message' : 'actors.delete_message', {
      name: actor.metadata.name,
    }),
    confirmText: t('substrate.delete'),
    success: t(actor.provider === 'proxmox' ? 'actors.deletion_requested' : 'actors.deleted', { name: actor.metadata.name }),
    run,
  };
}

/** Suspend/resume native-актора. У managed своих действий больше и у каждого
 * своя причина недоступности — их план строит managedActionPlan. */
export function actorActions(actor: Actor): ActorAction[] {
  if (actor.provider === 'proxmox') return [];
  const state = actorState(actor);
  return state === 'running' ? ['suspend'] : state === 'suspended' ? ['resume'] : [];
}

export interface NativePanelOptions {
  t: TFunction;
  /** owner/editor роя; viewer native-актора не открывает вовсе. */
  canManage: boolean;
  /** Идущее действие этого актора: его кнопка ждёт исхода. */
  pending?: ActorAction;
  terminal: string;
  onAction: (action: ActorAction) => void;
  onRemove: () => void;
}

/** Группы панели native-актора (#157) — те же действия, что были в строке:
 * терминал и suspend/resume, удаление — отдельным необратимым блоком.
 * Главное — Resume у приостановленного, иначе терминал. Managed-актор
 * строит свои группы сам (managedActionGroups). */
export function nativeActionGroups(
  actor: Actor,
  { t, canManage, pending, terminal, onAction, onRemove }: NativePanelOptions
): PanelActionGroup[] {
  if (!canManage || actor.provider === 'proxmox') return [];
  const lifecycle = actorActions(actor);
  const asleep = lifecycle.includes('resume');
  const open: PanelAction = {
    id: 'terminal',
    label: t('actors.ops.terminal.label'),
    description: t('actors.native.terminal_hint'),
    tone: asleep ? undefined : 'primary',
    href: terminal,
  };
  const states = lifecycle.map(
    (action): PanelAction => ({
      id: action,
      label: t(`actors.${action}`),
      description: t(`actors.native.${action}_hint`),
      tone: action === 'resume' ? 'primary' : undefined,
      pending: pending === action,
      onSelect: () => onAction(action),
    })
  );
  return [
    { id: 'main', label: t('substrate.actions'), actions: asleep ? [...states, open] : [open, ...states] },
    {
      id: 'danger',
      label: t('substrate.panel_danger'),
      danger: true,
      actions: [
        {
          id: 'delete',
          label: t('substrate.delete'),
          description: t('actors.native.delete_hint'),
          tone: 'danger',
          onSelect: onRemove,
        },
      ],
    },
  ];
}

// Виртуальный профиль не принимает sourceTag и native atespace из формы.
// Origin есть только у managed-актора; пустой не отправляется — тогда сервер
// берёт значение по умолчанию (repository проекта или пустой workspace).
export function actorCreateDraft(
  swarm: string,
  name: string,
  template: ActorTemplate,
  labels: Record<string, string>,
  sourceTag: string,
  origin = ''
): Actor {
  const workerSelector = Object.keys(labels).length ? { matchLabels: labels } : undefined;
  if (template.provider === 'proxmox') {
    const requested = origin.trim();
    return {
      metadata: { name },
      actorTemplate: { name: template.metadata.name },
      ...(workerSelector ? { workerSelector } : {}),
      ...(requested ? { origin: requested } : {}),
    };
  }
  return {
    metadata: { name },
    actorTemplate: { atespace: swarm, name: template.metadata.name },
    workerSelector,
    sourceTag: sourceTag ? { atespace: swarm, name: sourceTag } : undefined,
  };
}

// Замыкание не вызывает API до явного подтверждения пользователя.
export function resetConfirmation(actor: Actor, t: TFunction, run: () => Promise<unknown>) {
  return {
    title: t('actors.reset_title'),
    message: t('actors.reset_message', { name: actor.metadata.name }),
    confirmText: t('actors.reset'),
    success: t('actors.reset_done', { name: actor.metadata.name }),
    run,
  };
}

export interface ActionDialogText {
  title: string;
  /** Что будет потеряно и что сохранится. */
  message: string;
  /** Уточнение к последствию (например, откуда возьмётся новый workspace). */
  note?: string;
  confirmText: string;
}

const noop = async () => {};

/** Текст подтверждения managed-действия называет последствие именно этого
 * действия. Reset и delete — те же тексты, что и прежде (единственный
 * источник); reset говорит, будет ли новый workspace клонирован заново. */
export function actionDialogText(
  actor: Actor,
  id: 'restart' | 'recycle' | 'wipe' | 'reset' | 'delete',
  t: TFunction
): ActionDialogText {
  const name = actor.metadata.name;
  if (id === 'reset') {
    const { title, message, confirmText } = resetConfirmation(actor, t, noop);
    const note = actor.origin
      ? t('actors.ops.reset.origin_set', { origin: actor.origin })
      : t('actors.ops.reset.origin_empty');
    return { title, message, note, confirmText };
  }
  if (id === 'delete') {
    const { title, message, confirmText } = deleteConfirmation(actor, t, noop);
    return { title, message, confirmText };
  }
  return {
    title: t(`actors.ops.${id}.title`, { name }),
    message: t(`actors.ops.${id}.message`, { name }),
    confirmText: t(`actors.ops.${id}.label`),
  };
}
