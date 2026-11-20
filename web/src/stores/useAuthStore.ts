import { create } from 'zustand';
import { persist, createJSONStorage } from 'zustand/middleware';
import type { AuthState, LoginCredentials, ConnectionStatus } from '@/types';
import { STORAGE_KEY_AUTH } from '@/utils/constants';
import { obfuscatedStorage } from '@/services/storage/secureStorage';
import { unsavedDrafts } from '@/services/storage/unsavedDrafts';
import { apiClient } from '@/services/api/client';
import * as broodSession from '@/services/brood/session';
import { useConfigStore } from './useConfigStore';
import { useModelsStore } from './useModelsStore';
import { useQuotaStore } from './useQuotaStore';
import { detectApiBaseFromLocation } from '@/utils/connection';

interface AuthStoreState extends AuthState {
  connectionStatus: ConnectionStatus;
  login: (credentials: LoginCredentials) => Promise<void>;
  logout: () => void;
  checkAuth: () => Promise<boolean>;
  restoreSession: () => Promise<boolean>;
  updateServerVersion: (version: string | null, buildDate?: string | null) => void;
  updateServerPluginSupport: (supportsPlugin: boolean) => void;
}

let restoreSessionPromise: Promise<boolean> | null = null;
let generation = 0;

function clearCaches(): void {
  useConfigStore.getState().clearCache();
  useModelsStore.getState().clearCache();
  useQuotaStore.getState().clearQuotaCache();
}

function disconnect(): void {
  apiClient.setConfig({ apiBase: '', managementKey: '' });
  clearCaches();
  // Черновики редакторов принадлежат сессии пользователя, как и кэши.
  unsavedDrafts.clear();
  useAuthStore.setState({
    isAuthenticated: false,
    username: '',
    isSuperuser: false,
    apiBase: '',
    managementKey: '',
    serverVersion: null,
    serverBuildDate: null,
    supportsPlugin: false,
    connectionStatus: 'disconnected',
  });
}

function connect(session: broodSession.BroodSession): void {
  const apiBase = detectApiBaseFromLocation();
  const previous = useAuthStore.getState();
  if (previous.username !== session.username || previous.isSuperuser !== session.isSuperuser) {
    clearCaches();
  }
  apiClient.setConfig({ apiBase, managementKey: '' });
  useAuthStore.setState({
    isAuthenticated: true,
    username: session.username,
    isSuperuser: session.isSuperuser,
    apiBase,
    managementKey: '',
    connectionStatus: 'connected',
  });
}

export const useAuthStore = create<AuthStoreState>()(
  persist(
    (set, get) => ({
      isAuthenticated: false,
      username: '',
      isSuperuser: false,
      apiBase: '',
      managementKey: '',
      rememberPassword: false,
      serverVersion: null,
      serverBuildDate: null,
      supportsPlugin: false,
      connectionStatus: 'disconnected',

      restoreSession: () => {
        if (restoreSessionPromise) return restoreSessionPromise;
        const started = generation;
        restoreSessionPromise = (async () => {
          try {
            const session = await broodSession.getSession();
            if (started !== generation) return false;
            if (!session.authenticated) {
              disconnect();
              return false;
            }
            connect(session);
            return true;
          } catch {
            if (started === generation) disconnect();
            return false;
          } finally {
            if (started === generation) restoreSessionPromise = null;
          }
        })();
        return restoreSessionPromise;
      },

      login: async (credentials) => {
        const started = ++generation;
        restoreSessionPromise = null;
        disconnect();
        const session = await broodSession.login(credentials.username.trim(), credentials.password);
        if (started !== generation) return;
        if (!session.authenticated)
          throw new broodSession.BroodAuthError(401, 'authentication required');
        connect(session);
      },

      logout: () => {
        generation++;
        restoreSessionPromise = null;
        broodSession.logout().catch(() => undefined);
        disconnect();
      },

      checkAuth: () => get().restoreSession(),
      updateServerVersion: (version, buildDate) =>
        set({
          serverVersion: version || null,
          serverBuildDate: buildDate || null,
        }),
      updateServerPluginSupport: (supportsPlugin) => set({ supportsPlugin }),
    }),
    {
      name: STORAGE_KEY_AUTH,
      storage: createJSONStorage(() => ({
        getItem: (name) => {
          const data = obfuscatedStorage.getItem<AuthStoreState>(name);
          return data ? JSON.stringify(data) : null;
        },
        setItem: (name, value) => obfuscatedStorage.setItem(name, JSON.parse(value)),
        removeItem: (name) => obfuscatedStorage.removeItem(name),
      })),
      partialize: (state) => ({
        apiBase: state.apiBase,
        serverVersion: state.serverVersion,
        serverBuildDate: state.serverBuildDate,
      }),
    }
  )
);

if (typeof window !== 'undefined') {
  window.addEventListener('unauthorized', () => useAuthStore.getState().logout());
  window.addEventListener('server-version-update', ((event: CustomEvent) => {
    const detail = event.detail || {};
    useAuthStore.getState().updateServerVersion(detail.version || null, detail.buildDate || null);
  }) as EventListener);
  window.addEventListener('server-plugin-support-update', ((event: CustomEvent) => {
    useAuthStore.getState().updateServerPluginSupport(event.detail?.supportsPlugin === true);
  }) as EventListener);
}
