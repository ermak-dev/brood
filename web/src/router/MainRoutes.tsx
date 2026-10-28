import type { ReactNode } from 'react';
import { Navigate, useRoutes, useSearchParams, type Location } from 'react-router-dom';
import { AccessProjectsPage } from '@/features/access/ProjectsPage';
import { SwarmsPage } from '@/features/access/SwarmsPage';
import { UsersPage } from '@/features/access/UsersPage';
import { PermissionsPage } from '@/features/access/PermissionsPage';
import { useAdministrator } from '@/features/access/shared';
import { visibleAccessPath } from '@/features/access/permissions';
import { projectSectionPath } from '@/features/access/state';
import { DashboardPage } from '@/features/dashboard/DashboardPage';
import { ActorsPage } from '@/features/substrate/ActorsPage';
import { PolicyPage } from '@/features/substrate/PolicyPage';
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

/** Старые адреса рои-и-участники выбирали проект параметром запроса; теперь
 * проект — часть маршрута, и прежняя ссылка ведёт в его раздел. */
function LegacyProjectRoute({ section }: { section: 'swarms' | 'members' }) {
  const [params] = useSearchParams();
  return <Navigate to={projectSectionPath(section, params.get('project'))} replace />;
}

const createMainRoutes = (supportsPlugin: boolean) => [
  { path: '/', element: <DashboardPage /> },
  { path: '/dashboard', element: <DashboardPage /> },
  // Проекты brood и их рои — отдельные сущности. Рой и его atespace —
  // одна сущность интерфейса, поэтому отдельной страницы областей нет:
  // старый адрес ведёт к роям (docs/MODEL.md). Проект выбирается маршрутом,
  // а не вторым выпадающим списком рядом с глобальным выбором роя.
  { path: '/projects', element: <AccessProjectsPage /> },
  { path: '/projects/:project', element: <Navigate to="swarms" replace /> },
  { path: '/projects/:project/swarms', element: <SwarmsPage /> },
  { path: '/projects/:project/members', element: <PermissionsPage /> },
  { path: '/users', element: <UsersPage /> },
  { path: '/swarms', element: <LegacyProjectRoute section="swarms" /> },
  { path: '/permissions', element: <LegacyProjectRoute section="members" /> },
  { path: '/substrate-projects', element: <Navigate to="/projects" replace /> },
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
  return isSuperuser ? children : <Navigate to="/projects" replace />;
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
