import type { TFunction } from 'i18next';
import type { AccessSwarm } from '@/services/brood/access';
import type { PanelAction, PanelActionGroup } from '@/features/substrate/components/EntityPanel';
import { swarmCapabilities } from './shardState';

/** brood: какие действия предлагает панель объекта доступа (#157) и где.
 * План решает только видимость, порядок и необратимость -- по тем же
 * условиям, что прежние кнопки строки; подписи, подтверждения и обработчики
 * даёт страница. Сервер всё равно проверяет каждое действие. */
export interface PanelPlan<Id extends string> {
  /** Обычные действия по порядку; первое -- главное действие объекта. */
  main: Id[];
  /** Необратимые: отдельный блок внизу панели. */
  danger: Id[];
}

/** Что страница знает о действии: подпись, пояснение, обработчик, ожидание. */
export type ActionSpec = Omit<PanelAction, 'id' | 'tone'>;

export function panelGroups<Id extends string>(
  plan: PanelPlan<Id>,
  specs: Record<Id, ActionSpec>,
  t: TFunction
): PanelActionGroup[] {
  const groups: PanelActionGroup[] = [
    {
      id: 'main',
      label: t('substrate.actions'),
      actions: plan.main.map((id, index) => ({
        ...specs[id],
        id,
        tone: index === 0 ? 'primary' : undefined,
      })),
    },
    {
      id: 'danger',
      label: t('substrate.panel_danger'),
      danger: true,
      actions: plan.danger.map((id) => ({ ...specs[id], id, tone: 'danger' })),
    },
  ];
  return groups.filter((group) => group.actions.length > 0);
}

export type ProjectAction = 'select' | 'details' | 'delete';

/** Выбрать проект и открыть его сведения может любой, кому он виден;
 * удаляет только администратор. */
export function projectPlan(administrator: boolean): PanelPlan<ProjectAction> {
  return { main: ['select', 'details'], danger: administrator ? ['delete'] : [] };
}

export type SwarmAction = 'access' | 'retry_create' | 'transfer' | 'delete';

/** Доступ к рою виден у активного роя (не владелец читает его без правки);
 * повтор создания, передача и удаление -- по возможностям владельца.
 * Передача необратима для того, кто её делает: доступ прежнего владельца
 * заменяется. */
export function swarmPlan(swarm: AccessSwarm, administrator: boolean): PanelPlan<SwarmAction> {
  const can = swarmCapabilities(swarm, administrator);
  const main: SwarmAction[] = [];
  const danger: SwarmAction[] = [];
  if (swarm.state === 'active') main.push('access');
  if (can.retryCreate) main.push('retry_create');
  if (can.own) danger.push('transfer');
  if (can.remove) danger.push('delete');
  return { main, danger };
}

export type UserAction = 'edit' | 'password' | 'delete';

/** Страница пользователей открыта только администратору (маршрут AdminOnly),
 * поэтому набор действий постоянный. */
export function userPlan(): PanelPlan<UserAction> {
  return { main: ['edit', 'password'], danger: ['delete'] };
}

export type MemberAction = 'remove';

/** Состав проекта читают его участники; исключает только администратор. */
export function memberPlan(administrator: boolean): PanelPlan<MemberAction> {
  return { main: [], danger: administrator ? ['remove'] : [] };
}
