import type { ReactNode } from 'react';
import { Navigate, useRoutes, type Location } from 'react-router-dom';
import { useAdministrator } from '@/features/brood/hooks';
import { AgentsPage } from '@/features/brood/AgentsPage';
import { PoolsPage } from '@/features/brood/PoolsPage';
import { ProjectsPage } from '@/features/brood/ProjectsPage';
import { HOME_PATH, visiblePath } from '@/features/brood/routes';
import { SwarmsPage } from '@/features/brood/SwarmsPage';
import { UsersPage } from '@/features/brood/UsersPage';
import { DashboardPage } from '@/features/dashboard/DashboardPage';
import { ProfilePage } from '@/features/profile/ProfilePage';
import { PROFILE_PATH } from '@/features/profile/routes';
import { ProvidersWorkbenchPage } from '@/features/providers/ProvidersWorkbenchPage';
import { AuthFilesPage } from '@/features/authFiles/AuthFilesPage';
import { AuthFilesOAuthExcludedEditPage } from '@/pages/AuthFilesOAuthExcludedEditPage';
import { AuthFilesOAuthModelAliasEditPage } from '@/pages/AuthFilesOAuthModelAliasEditPage';
import { OAuthPage } from '@/features/oauth/OAuthPage';
import { QuotaPage } from '@/features/quota/QuotaPage';
import { PluginResourcePage } from '@/features/plugins/PluginResourcePage';
import { PluginsPage } from '@/features/plugins/PluginsPage';
import { PluginStorePage } from '@/features/plugins/PluginStorePage';
import { ConfigPage } from '@/features/config/ConfigPage';
import { LogsPage } from '@/features/logs/LogsPage';
import { SystemPage } from '@/pages/SystemPage';
import { useAuthStore } from '@/stores';

const createMainRoutes = (supportsPlugin: boolean) => [
  // Every entity is a flat table of its own; agents are the landing page.
  { path: '/', element: <Navigate to={HOME_PATH} replace /> },
  { path: '/dashboard', element: <DashboardPage /> },
  { path: PROFILE_PATH, element: <ProfilePage /> },
  { path: '/projects', element: <ProjectsPage /> },
  { path: '/swarms', element: <SwarmsPage /> },
  { path: '/agents', element: <AgentsPage /> },
  { path: '/pools', element: <PoolsPage /> },
  { path: '/users', element: <UsersPage /> },
  { path: '/ai-providers', element: <ProvidersWorkbenchPage /> },
  { path: '/auth-files', element: <AuthFilesPage /> },
  { path: '/auth-files/oauth-excluded', element: <AuthFilesOAuthExcludedEditPage /> },
  { path: '/auth-files/oauth-model-alias', element: <AuthFilesOAuthModelAliasEditPage /> },
  { path: '/oauth', element: <OAuthPage /> },
  { path: '/quota', element: <QuotaPage /> },
  ...(supportsPlugin
    ? [
        { path: '/plugin-pages/:pluginId/:menuIndex', element: <PluginResourcePage /> },
        { path: '/plugins', element: <PluginsPage /> },
        { path: '/plugin-store', element: <PluginStorePage /> },
      ]
    : []),
  { path: '/config', element: <ConfigPage /> },
  { path: '/logs', element: <LogsPage /> },
  { path: '/system', element: <SystemPage /> },
  { path: '*', element: <Navigate to="/" replace /> },
];

function AdminOnly({ children }: { children: ReactNode }) {
  const isSuperuser = useAdministrator();
  return isSuperuser ? children : <Navigate to={HOME_PATH} replace />;
}

export function MainRoutes({ location }: { location?: Location }) {
  const supportsPlugin = useAuthStore((state) => state.supportsPlugin);
  const routes = createMainRoutes(supportsPlugin).map((route) => ({
    ...route,
    element:
      visiblePath(route.path, false) || route.path === '*' ? (
        route.element
      ) : (
        <AdminOnly>{route.element}</AdminOnly>
      ),
  }));
  return useRoutes(routes, location);
}
