import type { ReactNode } from 'react';
import { Navigate, useRoutes, type Location } from 'react-router-dom';
import { useAdministrator } from '@/features/brood/hooks';
import { PoolsPage } from '@/features/brood/PoolsPage';
import { ProjectPage } from '@/features/brood/ProjectPage';
import { ProjectsPage } from '@/features/brood/ProjectsPage';
import { visiblePath } from '@/features/brood/routes';
import { SwarmPage } from '@/features/brood/SwarmPage';
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

/** Addresses of pages that no longer exist lead to the project list. */
const REMOVED_PATHS = [
  '/project',
  '/projects/:id/*',
  '/swarms',
  '/swarms/:id/*',
  '/permissions',
  '/substrate-projects',
  '/actors',
  '/templates',
  '/workers',
  '/worker-pools',
  '/workflows/*',
  '/pipelines/*',
  '/policy',
  '/substrate-policy',
];

const createMainRoutes = (supportsPlugin: boolean) => [
  // Navigation is the hierarchy Projects -> Project -> Swarm.
  { path: '/', element: <Navigate to="/projects" replace /> },
  { path: '/dashboard', element: <DashboardPage /> },
  { path: PROFILE_PATH, element: <ProfilePage /> },
  { path: '/projects', element: <ProjectsPage /> },
  { path: '/projects/:id', element: <ProjectPage /> },
  { path: '/swarms/:id', element: <SwarmPage /> },
  { path: '/pools', element: <PoolsPage /> },
  { path: '/users', element: <UsersPage /> },
  ...REMOVED_PATHS.map((path) => ({ path, element: <Navigate to="/projects" replace /> })),
  { path: '/settings', element: <Navigate to="/config" replace /> },
  { path: '/api-keys', element: <Navigate to="/config" replace /> },
  // brood: no sponsor quick start page; its old address goes to the providers.
  { path: '/quick-start', element: <Navigate to="/ai-providers" replace /> },
  { path: '/quick-start/*', element: <Navigate to="/ai-providers" replace /> },
  { path: '/ai-providers', element: <ProvidersWorkbenchPage /> },
  { path: '/ai-providers/*', element: <Navigate to="/ai-providers" replace /> },
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
        { path: '/plugins/*', element: <Navigate to="/plugins" replace /> },
      ]
    : [
        { path: '/plugin-pages/*', element: <Navigate to="/" replace /> },
        { path: '/plugins/*', element: <Navigate to="/" replace /> },
        { path: '/plugin-store', element: <Navigate to="/" replace /> },
      ]),
  { path: '/config', element: <ConfigPage /> },
  { path: '/logs', element: <LogsPage /> },
  { path: '/system', element: <SystemPage /> },
  { path: '*', element: <Navigate to="/" replace /> },
];

function AdminOnly({ children }: { children: ReactNode }) {
  const isSuperuser = useAdministrator();
  return isSuperuser ? children : <Navigate to="/projects" replace />;
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
