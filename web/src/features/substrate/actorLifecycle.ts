import type { TFunction } from 'i18next';
import type { Actor, ActorAction, ActorTemplate } from '@/services/brood/substrate';
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

export function actorActions(actor: Actor): ActorAction[] {
  const state = actorState(actor);
  if (actor.provider !== 'proxmox') {
    return state === 'running' ? ['suspend'] : state === 'suspended' ? ['resume'] : [];
  }
  if (actor.desired === 'deleted') return [];
  if (state === 'running') return ['stop', 'reset'];
  if (state === 'stopped') return ['resume', 'reset'];
  if (state === 'error') return ['resume', 'stop', 'reset'];
  return [];
}

// Виртуальный профиль не принимает sourceTag и native atespace из формы.
export function actorCreateDraft(
  swarm: string,
  name: string,
  template: ActorTemplate,
  labels: Record<string, string>,
  sourceTag: string
): Actor {
  const workerSelector = Object.keys(labels).length ? { matchLabels: labels } : undefined;
  if (template.provider === 'proxmox') {
    return {
      metadata: { name },
      actorTemplate: { name: template.metadata.name },
      ...(workerSelector ? { workerSelector } : {}),
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
