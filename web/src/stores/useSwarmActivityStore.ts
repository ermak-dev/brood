/**
 * Сводка роёв выбранного проекта (#255): список роёв и исполнение каждого,
 * одна на панель. Меню берёт отсюда имя проекта и число ждущих ответа,
 * список роёв -- исполнение и открытые вопросы, чтобы опрос по старым
 * маршрутам (swarmActivity.ts, до сводки сервера #234) шёл один раз.
 */
import { create } from 'zustand';
import {
  activityTargets,
  loadSwarmActivities,
  type SwarmActivity,
} from '@/features/access/swarmActivity';
import { accessApi, type AccessSwarm } from '@/services/brood/access';
import { errorMessage } from '@/services/brood/http';
import { pipelinesApi } from '@/services/brood/pipelines';
import { requestsApi } from '@/services/brood/requests';

interface SwarmActivityState {
  /** Проект, которому принадлежат данные; '' -- ничего не загружено. */
  project: string;
  /** Имя проекта из каталога; '' -- пока не известно. */
  name: string;
  swarms: AccessSwarm[] | null;
  activities: Record<string, SwarmActivity | null>;
  loading: boolean;
  error: string;
  /** Список и сводка проекта: смена проекта, обновление из шапки. */
  load: (project: string) => Promise<void>;
  /** Сводка по уже прочитанному списку: страница роёв после своей загрузки. */
  adopt: (project: string, swarms: AccessSwarm[]) => Promise<void>;
}

const source = {
  run: pipelinesApi.getSwarmRun,
  requests: requestsApi.list,
};

const empty = { project: '', name: '', swarms: null, activities: {}, loading: false, error: '' };

/** Тот же список: те же рои в тех же состояниях и ревизиях. */
const sameSwarms = (known: AccessSwarm[] | null, swarms: AccessSwarm[]) =>
  known !== null &&
  known.length === swarms.length &&
  known.every(
    (swarm, index) =>
      swarm.id === swarms[index].id &&
      swarm.state === swarms[index].state &&
      swarm.updated_at === swarms[index].updated_at
  );

/** Сводка, собранная только что, годится и странице: при входе на страницу
 * роёв меню и страница читают список почти одновременно, и второй опрос
 * (до 40 запросов) ничего нового не принёс бы. */
const FRESH_MS = 5000;

export const useSwarmActivityStore = create<SwarmActivityState>()((set, get) => {
  // Номер последнего запроса: ответ по прежнему проекту не ложится поверх.
  let latest = 0;
  let loadedAt = 0;
  const current = (request: number, project: string) =>
    request === latest && get().project === project;

  const aggregate = async (request: number, project: string, swarms: AccessSwarm[]) => {
    const activities = await loadSwarmActivities(activityTargets(swarms), source);
    if (current(request, project)) {
      loadedAt = Date.now();
      set({ activities, loading: false });
    }
  };

  const fresh = (project: string, swarms: AccessSwarm[]) => {
    const state = get();
    return (
      state.project === project &&
      sameSwarms(state.swarms, swarms) &&
      (state.loading || Date.now() - loadedAt < FRESH_MS)
    );
  };

  return {
    ...empty,
    load: async (project) => {
      const request = ++latest;
      if (!project) {
        set(empty);
        return;
      }
      set({ project, loading: true, error: '' });
      try {
        const [projects, swarms] = await Promise.all([
          accessApi.projects(),
          accessApi.swarms(project),
        ]);
        // Имя -- свойство проекта: годится, даже если список уже принесла
        // страница роёв (adopt) и этот запрос ей уступил.
        if (get().project === project) {
          set({ name: projects.find((item) => item.id === project)?.name ?? '' });
        }
        if (!current(request, project)) return;
        set({ swarms });
        await aggregate(request, project, swarms);
      } catch (error) {
        if (current(request, project)) {
          set({ swarms: null, activities: {}, loading: false, error: errorMessage(error) });
        }
      }
    },
    adopt: async (project, swarms) => {
      if (fresh(project, swarms)) {
        set({ swarms });
        return;
      }
      const request = ++latest;
      const name = get().project === project ? get().name : '';
      set({ project, name, swarms, loading: true, error: '' });
      await aggregate(request, project, swarms);
    },
  };
});
