/**
 * Список роёв выбранного проекта (#255), один на панель. Меню берёт отсюда
 * имя проекта и число ждущих ответа: исполнение и открытые вопросы несёт
 * сама строка списка (#234), поэтому сводка -- это список, без опроса роёв.
 */
import { create } from 'zustand';
import { accessApi, type AccessSwarm } from '@/services/brood/access';
import { errorMessage } from '@/services/brood/http';

interface SwarmActivityState {
  /** Проект, которому принадлежат данные; '' -- ничего не загружено. */
  project: string;
  /** Имя проекта из каталога; '' -- пока не известно. */
  name: string;
  swarms: AccessSwarm[] | null;
  loading: boolean;
  error: string;
  /** Список проекта: смена проекта, обновление из шапки. */
  load: (project: string) => Promise<void>;
  /** Уже прочитанный список: страница роёв после своей загрузки. */
  adopt: (project: string, swarms: AccessSwarm[]) => void;
}

const empty = { project: '', name: '', swarms: null, loading: false, error: '' };

export const useSwarmActivityStore = create<SwarmActivityState>()((set, get) => {
  // Номер последнего запроса: ответ по прежнему проекту или более старый
  // ответ того же проекта не ложится поверх.
  let latest = 0;
  const current = (request: number, project: string) =>
    request === latest && get().project === project;

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
        if (current(request, project)) set({ swarms, loading: false });
      } catch (error) {
        if (current(request, project)) {
          set({ swarms: null, loading: false, error: errorMessage(error) });
        }
      }
    },
    adopt: (project, swarms) => {
      ++latest;
      const name = get().project === project ? get().name : '';
      set({ project, name, swarms, loading: false, error: '' });
    },
  };
});
