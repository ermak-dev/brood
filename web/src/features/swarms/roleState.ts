/**
 * Чистая логика ролей роя на его странице (#241, API #235): кто меняет
 * привязки, из какого графа роли, каких акторов предложить и какие роли
 * запуск отвергнет. Сервер остаётся судьёй каждой привязки; панель только
 * не предлагает того, что он заведомо отвергнет.
 */
import { roleTone } from '@/features/access/state';
import { swarmCapabilities } from '@/features/access/shardState';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun } from '@/services/brood/pipelines';
import type { GraphRole, RoleActor, SwarmRolesView } from '@/services/brood/roles';
import { swarmExecution } from './swarmState';

/** edit — привязки меняются; busy — исполнение идёт или ждёт ответа, и
 * сервер их не меняет; read — зритель или неактивный рой. */
export type RoleControl = 'edit' | 'busy' | 'read';

const busyExecutions = new Set(['running', 'waiting']);

/** Что пользователь может сделать с привязками. Право — как у запуска:
 * owner, editor или администратор активного роя; исполнение — по
 * представлению ролей, которое сервер отдал вместе с ними. Пока его нет,
 * исполнение не известно, и предлагать нечего. */
export function roleControl(
  swarm: AccessSwarm,
  administrator: boolean,
  view: SwarmRolesView | null
): RoleControl {
  if (!view || !swarmCapabilities(swarm, administrator).manage) return 'read';
  return busyExecutions.has(view.execution) ? 'busy' : 'edit';
}

/** Определение, чьи роли спросить. У роя со снимком роли — его снимка, и
 * чужое имя сервер отверг бы; у роя без снимка — выбранного в диалоге
 * запуска; пока исполнение не прочитано, снимок не известен — без имени. */
export function rolesDefinition(run: PipelineRun | null | undefined, chosen: string): string {
  return run === null ? chosen : '';
}

/** Когда перечитать роли: сменилось исполнение или снимок. Опрос того же
 * состояния ключа не меняет. */
export function rolesRefreshKey(run: PipelineRun | null | undefined): string {
  if (run === undefined) return 'unknown';
  const snapshot = run?.definition;
  return [swarmExecution(run), snapshot?.name ?? '', snapshot?.revision ?? ''].join(':');
}

/** Когда перечитать акторов: сменилось состояние или актор какой-то роли.
 * Устаревшая роль значит, что актор пересоздан и инкарнация в списке
 * старая: привязка по ней была бы отвергнута. Тихий опрос ключа не меняет. */
export function actorsRefreshKey(view: SwarmRolesView | null): string {
  return (view?.roles ?? [])
    .map((role) => `${role.role}:${role.state}:${role.actorUid ?? ''}`)
    .join('|');
}

const livePhases = new Set(['provisioning', 'running']);

/** Акторы, которых сервер примет: managed этого роя в фазе provisioning
 * или running и с желаемым running. Остальных панель не предлагает. */
export function bindableActors(actors: RoleActor[] | null): RoleActor[] {
  return (actors ?? []).filter(
    (actor) => livePhases.has(actor.phase) && actor.desired === 'running'
  );
}

/** Выбранный для роли актор: выбор оператора, пока он предложен; иначе
 * привязанный актор (перепривязка устаревшей роли — к нему же); иначе
 * единственный. Из нескольких панель не выбирает за оператора, а выбор,
 * которого больше нет, не подменяется другим актором. */
export function actorChoice(
  actors: RoleActor[],
  role: GraphRole,
  chosen: string | undefined
): RoleActor | undefined {
  if (chosen !== undefined) return actors.find((actor) => actor.uid === chosen);
  const bound = actors.find((actor) => actor.uid === role.actorUid);
  if (bound) return bound;
  return actors.length === 1 ? actors[0] : undefined;
}

/** Роли графа, которые запуск отвергнет; привязки вне графа ему не мешают. */
export function unreadyRoles(view: SwarmRolesView | null): GraphRole[] {
  return (view?.roles ?? []).filter((role) => role.inGraph && role.state !== 'ready');
}

/** Тон роли живёт в общем словаре тонов (#254). */
export { roleTone };
