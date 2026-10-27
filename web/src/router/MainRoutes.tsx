import type { ReactNode } from 'react';
import { Navigate, useRoutes, type Location } from 'react-router-dom';
import { AccessProjectsPage } from '@/features/access/ProjectsPage';
import { SwarmsPage } from '@/features/access/SwarmsPage';
import { UsersPage } from '@/features/access/UsersPage';
import { PermissionsPage } from '@/features/access/PermissionsPage';
import { useAdministrator } from '@/features/access/shared';
import { visibleAccessPath } from '@/features/access/permissions';
import { DashboardPage } from '@/features/dashboard/DashboardPage';
import { ActorsPage } from '@/features/substrate/ActorsPage';
import { PolicyPage } from '@/features/substrate/PolicyPage';
import { ProjectsPage } from '@/features/substrate/ProjectsPage';
import { TemplatesPage } from '@/features/substrate/TemplatesPage';
import { WorkersPage } from '@/features/substrate/WorkersPage';
import { WorkerPoolsPage } from '@/features/substrate/WorkerPoolsPage';
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
  { path: '/', element: <DashboardPage /> },
  { path: '/dashboard', element: <DashboardPage /> },
  // Проекты brood и их рои — отдельные сущности. Исходная страница
  // областей Substrate сохранена в администраторском разделе.
  { path: '/projects', element: <AccessProjectsPage /> },
  { path: '/swarms', element: <SwarmsPage /> },
  { path: '/users', element: <UsersPage /> },
  { path: '/permissions', element: <PermissionsPage /> },
  { path: '/substrate-projects', element: <ProjectsPage /> },
  { path: '/actors', element: <ActorsPage /> },
  { path: '/templates', element: <TemplatesPage /> },
  { path: '/workers', element: <WorkersPage /> },
  { path: '/worker-pools', element: <WorkerPoolsPage /> },
  { path: '/policy', element: <PolicyPage /> },
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
  return isSuperuser ? children : <Navigate to="/swarms" replace />;
}

export function MainRoutes({ location }: { location?: Location }) {
  const supportsPlugin = useAuthStore((state) => state.supportsPlugin);
  const routes = createMainRoutes(supportsPlugin).map((route) => ({
    ...route,
    element:
      visibleAccessPath(route.path, false) || route.path === '*' ? (
        route.element
      ) : (
        <AdminOnly>{route.element}</AdminOnly>
      ),
  }));
  return useRoutes(routes, location);
}
