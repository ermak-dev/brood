/** Active swarm selection is a per-user preference, never an access grant. */
import { create } from 'zustand';
import { persist } from 'zustand/middleware';

interface ProjectState {
  project: string;
  selectedFor: string;
  setProject: (project: string) => void;
  bindUser: (username: string) => void;
}

export const useProjectStore = create<ProjectState>()(
  persist(
    (set) => ({
      project: '',
      selectedFor: '',
      setProject: (project) => set({ project }),
      bindUser: (username) =>
        set((state) => ({
          selectedFor: username,
          project: username && state.selectedFor === username ? state.project : '',
        })),
    }),
    { name: 'brood-swarm' }
  )
);
