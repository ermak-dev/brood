/**
 * 认证状态管理
 * 从原项目 src/modules/login.js 和 src/core/connection.js 迁移
 */

import { create } from 'zustand';
import { persist, createJSONStorage } from 'zustand/middleware';
import type { AuthState, LoginCredentials, ConnectionStatus } from '@/types';
import { STORAGE_KEY_AUTH } from '@/utils/constants';
import { obfuscatedStorage } from '@/services/storage/secureStorage';
import { apiClient } from '@/services/api/client';
import * as broodSession from '@/services/brood/session';
import { useConfigStore } from './useConfigStore';
import { useModelsStore } from './useModelsStore';
import { useQuotaStore } from './useQuotaStore';
import { detectApiBaseFromLocation } from '@/utils/connection';

interface AuthStoreState extends AuthState {
  connectionStatus: ConnectionStatus;

  // 操作
  login: (credentials: LoginCredentials) => Promise<void>;
  logout: () => void;
  checkAuth: () => Promise<boolean>;
  restoreSession: () => Promise<boolean>;
  updateServerVersion: (version: string | null, buildDate?: string | null) => void;
  updateServerPluginSupport: (supportsPlugin: boolean) => void;
}

let restoreSessionPromise: Promise<boolean> | null = null;

// Connect the API client to this origin and prove the session reaches the
// management API: the first config fetch is the check.
async function connect(): Promise<void> {
  const apiBase = detectApiBaseFromLocation();
  const set = useAuthStore.setState;
  try {
    set({
      connectionStatus: 'connecting',
      serverVersion: null,
      serverBuildDate: null,
      supportsPlugin: false,
    });
    useConfigStore.getState().clearCache();
    useModelsStore.getState().clearCache();
    useQuotaStore.getState().clearQuotaCache();
    apiClient.setConfig({ apiBase, managementKey: '' });
    await useConfigStore.getState().fetchConfig(true);
    set({ isAuthenticated: true, apiBase, managementKey: '', connectionStatus: 'connected' });
  } catch (error: unknown) {
    set({ connectionStatus: 'error' });
    throw error;
  }
}

export const useAuthStore = create<AuthStoreState>()(
  persist(
    (set, get) => ({
      // 初始状态
      isAuthenticated: false,
      apiBase: '',
      managementKey: '',
      rememberPassword: false,
      serverVersion: null,
      serverBuildDate: null,
      supportsPlugin: false,
      connectionStatus: 'disconnected',

      // brood: the session lives in the brood server's cookie, so restoring it is asking
      // the server. The API base is always this page's origin: the brood edge proxies
      // /v8/management to CLIProxyAPI and adds the management key there.
      restoreSession: () => {
        if (restoreSessionPromise) return restoreSessionPromise;

        restoreSessionPromise = (async () => {
          try {
            const session = await broodSession.getSession();
            if (!session.authenticated) return false;
            await connect();
            return true;
          } catch (error) {
            console.warn('Session restore failed:', error);
            return false;
          } finally {
            restoreSessionPromise = null;
          }
        })();

        return restoreSessionPromise;
      },

      login: async (credentials) => {
        await broodSession.login(credentials.username.trim(), credentials.password);
        await connect();
      },

      logout: () => {
        restoreSessionPromise = null;
        broodSession.logout().catch(() => undefined);
        apiClient.setConfig({ apiBase: '', managementKey: '' });
        useConfigStore.getState().clearCache();
        useModelsStore.getState().clearCache();
        useQuotaStore.getState().clearQuotaCache();
        set({
          isAuthenticated: false,
          apiBase: '',
          managementKey: '',
          serverVersion: null,
          serverBuildDate: null,
          supportsPlugin: false,
          connectionStatus: 'disconnected',
        });
        localStorage.removeItem('isLoggedIn');
      },

      checkAuth: async () => {
        const { apiBase } = get();
        if (!apiBase) return false;

        try {
          apiClient.setConfig({ apiBase, managementKey: '' });
          set({ supportsPlugin: false });
          await useConfigStore.getState().fetchConfig();
          set({ isAuthenticated: true, connectionStatus: 'connected' });
          return true;
        } catch {
          set({ isAuthenticated: false, connectionStatus: 'error', supportsPlugin: false });
          return false;
        }
      },

      // 更新服务器版本
      updateServerVersion: (version, buildDate) => {
        set({
          serverVersion: version || null,
          serverBuildDate: buildDate || null,
        });
      },

      updateServerPluginSupport: (supportsPlugin) => {
        set({ supportsPlugin });
      },
    }),
    {
      name: STORAGE_KEY_AUTH,
      storage: createJSONStorage(() => ({
        getItem: (name) => {
          const data = obfuscatedStorage.getItem<AuthStoreState>(name);
          return data ? JSON.stringify(data) : null;
        },
        setItem: (name, value) => {
          obfuscatedStorage.setItem(name, JSON.parse(value));
        },
        removeItem: (name) => {
          obfuscatedStorage.removeItem(name);
        },
      })),
      // No secret is persisted: the session is the server's cookie.
      partialize: (state) => ({
        apiBase: state.apiBase,
        serverVersion: state.serverVersion,
        serverBuildDate: state.serverBuildDate,
      }),
    }
  )
);

// 监听全局未授权事件
if (typeof window !== 'undefined') {
  window.addEventListener('unauthorized', () => {
    useAuthStore.getState().logout();
  });

  window.addEventListener('server-version-update', ((e: CustomEvent) => {
    const detail = e.detail || {};
    useAuthStore.getState().updateServerVersion(detail.version || null, detail.buildDate || null);
  }) as EventListener);

  window.addEventListener('server-plugin-support-update', ((e: CustomEvent) => {
    useAuthStore.getState().updateServerPluginSupport(e.detail?.supportsPlugin === true);
  }) as EventListener);
}
