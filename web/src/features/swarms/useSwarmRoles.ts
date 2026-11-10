/**
 * Роли роя на его странице (#241): представление ролей, акторы-кандидаты
 * и действия над привязками. Ответ записи — представление после неё, оно
 * ложится сразу; отказ остаётся у своей роли причиной сервера.
 */
import { useCallback, useEffect, useRef, useState } from 'react';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useInterval } from '@/hooks/useInterval';
import type { AccessSwarm } from '@/services/brood/access';
import { errorMessage } from '@/services/brood/http';
import type { PipelineRun } from '@/services/brood/pipelines';
import { rolesApi, type RoleActor, type SwarmRolesView } from '@/services/brood/roles';
import {
  actorsRefreshKey,
  bindableActors,
  roleControl,
  rolesDefinition,
  rolesRefreshKey,
  type RoleControl,
} from './roleState';

/** Итог последнего действия над ролью: кому привязана, снята или отказ.
 * Живёт до следующего действия над той же ролью. */
export interface RoleNotice {
  bound?: string;
  unbound?: boolean;
  refusal?: string;
}

export interface SwarmRolesModel {
  /** Представление ролей; null — ещё не прочитано. */
  view: SwarmRolesView | null;
  error: string;
  refresh: () => Promise<void>;
  /** Определение каталога, чьи роли спрошены; '' — снимок или без графа. */
  definition: string;
  control: RoleControl;
  /** Акторы, которых сервер примет; null — не прочитаны или не нужны. */
  actors: RoleActor[] | null;
  actorsError: string;
  refreshActors: () => Promise<void>;
  notices: Record<string, RoleNotice>;
  /** Роль, чья запись идёт; другие записи ждут её конца. */
  pending: string | null;
  /** Роль, чьему итогу перейти фокус: нажатая кнопка на время записи
   * занята и теряет фокус, а снятая привязка уносит её вовсе. */
  focus: string | null;
  focused: () => void;
  bind: (role: string, actor: RoleActor) => Promise<void>;
  unbind: (role: string) => Promise<void>;
}

/** Роли перечитываются, пока рой активен: актор стартует или
 * останавливается сам, и состояние роли меняется без действия здесь. */
const POLL_MS = 5000;

/** Последнее чтение побеждает: ответ, начатый раньше записи или смены
 * определения, не ложится поверх нового. */
function useLatest<T>(load: () => Promise<T>) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState('');
  const latest = useRef(0);
  const refresh = useCallback(async () => {
    const call = ++latest.current;
    try {
      const loaded = await load();
      if (call !== latest.current) return;
      setData(loaded);
      setError('');
    } catch (failure) {
      if (call !== latest.current) return;
      setError(errorMessage(failure));
    }
  }, [load]);
  const put = useCallback((value: T) => {
    latest.current += 1;
    setData(value);
    setError('');
  }, []);
  return { data, error, refresh, put };
}

export function useSwarmRoles(
  swarm: AccessSwarm,
  administrator: boolean,
  run: PipelineRun | null | undefined,
  chosen: string
): SwarmRolesModel {
  const swarmId = swarm.id;
  const definition = rolesDefinition(run, chosen);
  const loadView = useCallback(() => rolesApi.list(swarmId, definition), [swarmId, definition]);
  const roles = useLatest(loadView);
  const control = roleControl(swarm, administrator, roles.data);
  const editing = control === 'edit';
  const loadActors = useCallback(() => rolesApi.actors(swarmId), [swarmId]);
  const actors = useLatest(loadActors);
  const [notices, setNotices] = useState<Record<string, RoleNotice>>({});
  const [pending, setPending] = useState<string | null>(null);
  const [focus, setFocus] = useState<string | null>(null);
  const lock = useRef(false);

  const key = rolesRefreshKey(run);
  const refreshView = roles.refresh;
  useEffect(() => {
    void refreshView();
  }, [refreshView, key]);
  // Кандидаты нужны только тому, кто сейчас может привязать; перечитываются,
  // когда роль сменила состояние или актора (пересозданный актор — новая
  // инкарнация).
  const refreshActors = actors.refresh;
  const actorsKey = actorsRefreshKey(roles.data);
  useEffect(() => {
    if (editing) void refreshActors();
  }, [editing, refreshActors, actorsKey]);
  const refreshAll = useCallback(async () => {
    await Promise.all([refreshView(), editing ? refreshActors() : undefined]);
  }, [editing, refreshActors, refreshView]);
  useHeaderRefresh(refreshAll);
  useInterval(() => void refreshView(), swarm.state === 'active' ? POLL_MS : null);

  const note = (role: string, notice: RoleNotice) =>
    setNotices((all) => ({ ...all, [role]: notice }));
  const focused = useCallback(() => setFocus(null), []);

  const write = async (role: string, mutation: () => Promise<SwarmRolesView>) => {
    if (lock.current) return null;
    lock.current = true;
    setPending(role);
    note(role, {});
    try {
      const view = await mutation();
      roles.put(view);
      return view;
    } catch (failure) {
      // Отказ часто значит, что рой или актор уже другие: перечитать оба,
      // чтобы следующая попытка шла по нынешнему состоянию.
      note(role, { refusal: errorMessage(failure) });
      setFocus(role);
      void refreshAll();
      return null;
    } finally {
      lock.current = false;
      setPending(null);
    }
  };

  const bind = async (role: string, actor: RoleActor) => {
    const view = await write(role, () => rolesApi.bind(swarmId, role, actor));
    if (!view) return;
    const bound = view.roles.find((item) => item.role === role);
    note(role, { bound: bound?.actorName || actor.name });
    setFocus(role);
  };

  const unbind = async (role: string) => {
    if (!(await write(role, () => rolesApi.unbind(swarmId, role)))) return;
    note(role, { unbound: true });
    setFocus(role);
  };

  return {
    view: roles.data,
    error: roles.error,
    refresh: refreshView,
    definition,
    control,
    actors: editing && actors.data ? bindableActors(actors.data) : null,
    actorsError: editing ? actors.error : '',
    refreshActors,
    notices,
    pending,
    focus,
    focused,
    bind,
    unbind,
  };
}
