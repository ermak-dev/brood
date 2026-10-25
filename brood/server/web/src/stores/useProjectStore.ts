/**
 * brood: активный проект (atespace) панели.
 *
 * Глобальный переключатель в шапке; страницы сущностей проекта (акторы,
 * шаблоны, политика) читают его отсюда. Живёт в localStorage -- выбор
 * переживает перезагрузку, но не сессию сервера.
 */

import { create } from 'zustand';
import { persist } from 'zustand/middleware';

const STORAGE_KEY_PROJECT = 'brood-project';

interface ProjectState {
  project: string;
  setProject: (project: string) => void;
}

export const useProjectStore = create<ProjectState>()(
  persist(
    (set) => ({
      project: '',
      setProject: (project) => set({ project }),
    }),
    { name: STORAGE_KEY_PROJECT }
  )
);
