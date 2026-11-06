import { useEffect, type ReactNode } from 'react';
import { Navigate, useParams, useRoutes, useSearchParams, type Location } from 'react-router-dom';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { AccessProjectsPage } from '@/features/access/ProjectsPage';
import { SwarmsPage } from '@/features/access/SwarmsPage';
import { UsersPage } from '@/features/access/UsersPage';
import { ProjectDetailPage } from '@/features/access/ProjectDetailPage';
import { useAdministrator } from '@/features/access/shared';
import { visibleAccessPath } from '@/features/access/permissions';
import { DashboardPage } from '@/features/dashboard/DashboardPage';
import { ActorsPage } from '@/features/substrate/ActorsPage';
import { PolicyPage } from '@/features/substrate/PolicyPage';
import { TemplatesPage } from '@/features/substrate/TemplatesPage';
import { WorkersPage } from '@/features/substrate/WorkersPage';
import { WorkerPoolsPage } from '@/features/substrate/WorkerPoolsPage';
import { PipelinesPage } from '@/features/pipelines/PipelinesPage';
import { RunDetailPage } from '@/features/pipelines/RunDetailPage';
import { legacyPipelinePath } from '@/features/pipelines/routes';
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
import { useAuthStore, useProjectStore } from '@/stores';

/** Старые ссылки выбирают тот же проект в глобальном контексте и переходят
 * в канонический раздел без второго независимого выбора. */
function LegacyProjectRoute({ section }: { section: 'swarms' | 'members' | 'detail' }) {
  const [params] = useSearchParams();
  const route = useParams();
  const requested = route.project || params.get('project');
  const selected = useProjectStore((state) => state.project);
  const setProject = useProjectStore((state) => state.setProject);
  useEffect(() => {
    if (requested) setProject(requested);
  }, [requested, setProject]);
  if (requested && selected !== requested) return <LoadingSpinner />;
  if (section === 'detail') return <ProjectDetailPage />;
  if (requested)
    return (
      <Navigate
        to={section === 'swarms' ? '/swarms' : `/projects/${encodeURIComponent(requested)}`}
        replace
      />
    );
  if (section === 'members' && selected)
    return <Navigate to={`/projects/${encodeURIComponent(selected)}`} replace />;
  return section === 'swarms' ? <SwarmsPage /> : <ProjectDetailPage />;
}

function LegacyPipelineRoute() {
  const { id } = useParams();
  const [params] = useSearchParams();
  const search = params.toString() ? `?${params.toString()}` : '';
  return <Navigate to={legacyPipelinePath(id, search)} replace />;
}

const createMainRoutes = (supportsPlugin: boolean) => [
  { path: '/', element: <DashboardPage /> },
  { path: '/dashboard', element: <DashboardPage /> },
  // Проект — глобальный контекст; прежние адреса сохраняют выбор проекта.
  { path: '/project', element: <ProjectDetailPage /> },
  { path: '/projects', element: <AccessProjectsPage /> },
  { path: '/projects/:project', element: <LegacyProjectRoute section="detail" /> },
  { path: '/projects/:project/swarms', element: <LegacyProjectRoute section="swarms" /> },
  { path: '/projects/:project/members', element: <LegacyProjectRoute section="members" /> },
  { path: '/users', element: <UsersPage /> },
  { path: '/swarms', element: <LegacyProjectRoute section="swarms" /> },
  { path: '/permissions', element: <LegacyProjectRoute section="members" /> },
  { path: '/substrate-projects', element: <Navigate to="/projects" replace /> },
  { path: '/actors', element: <ActorsPage /> },
  { path: '/templates', element: <TemplatesPage /> },
  { path: '/workers', element: <WorkersPage /> },
  { path: '/worker-pools', element: <WorkerPoolsPage /> },
  { path: '/workflows', element: <PipelinesPage /> },
  { path: '/workflows/runs/:id', element: <RunDetailPage /> },
  { path: '/pipelines', element: <LegacyPipelineRoute /> },
  { path: '/pipelines/:id', element: <LegacyPipelineRoute /> },
  { path: '/policy', element: <LegacyProjectRoute section="members" /> },
  { path: '/substrate-policy', element: <PolicyPage /> },
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
