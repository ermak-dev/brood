import type { ComponentType } from 'react';
import {
  IconSidebarActors,
  IconSidebarAuthFiles,
  IconSidebarConfig,
  IconSidebarDashboard,
  IconSidebarLogs,
  IconSidebarOauth,
  IconSidebarOverview,
  IconSidebarPlugins,
  IconSidebarPolicy,
  IconSidebarProjectList,
  IconSidebarProjects,
  IconSidebarProviders,
  IconSidebarQuota,
  IconSidebarStore,
  IconSidebarSwarms,
  IconSidebarSystem,
  IconSidebarTemplates,
  IconSidebarUsers,
  IconSidebarWorkerPools,
  IconSidebarWorkers,
  type IconProps,
} from '@/components/ui/icons';
import { visibleAccessPath } from '@/features/access/permissions';

export function isNavigationActive(path: string, pathname: string, routerActive: boolean): boolean {
  if (/^\/projects\/[^/]+\/?$/.test(pathname)) {
    if (path === '/project') return true;
    if (path === '/projects') return false;
  }
  return routerActive;
}

export interface NavEntry {
  path: string;
  labelKey: string;
  metaKey?: string;
  icon: ComponentType<IconProps>;
  /** Счётчик у пункта (#255: рои, ждущие ответа) и ключ его подписи для
   * читалки с {{count}}; без счётчика пункт как прежде. */
  badge?: number;
  badgeLabelKey?: string;
}

export interface NavSection {
  id: 'project' | 'infrastructure' | 'administration' | 'gateway' | 'system';
  labelKey: string;
  /** Готовая подпись вместо ключа: группа проекта названа его именем. */
  label?: string;
  entries: NavEntry[];
}

// Группа — уровень объекта: ресурсы выбранного проекта, инфраструктура
// установки, администрирование, AI-шлюз и сама система. Видимость решает
// visibleAccessPath: участнику остаются проект с его ресурсами и каталог
// проектов, остальное — администратору; сервер проверяет каждый запрос.
// Обзор проекта открывает панель (#197), статистика шлюза живёт в его группе.
const sections = (plugins: boolean): NavSection[] => [
  {
    id: 'project',
    labelKey: 'nav_groups.project',
    entries: [
      {
        path: '/',
        labelKey: 'nav.overview',
        metaKey: 'nav_meta.overview',
        icon: IconSidebarOverview,
      },
      {
        path: '/project',
        labelKey: 'nav.project',
        metaKey: 'nav_meta.project',
        icon: IconSidebarProjects,
      },
      { path: '/swarms', labelKey: 'access.swarms', icon: IconSidebarSwarms },
      {
        path: '/actors',
        labelKey: 'nav.actors',
        metaKey: 'nav_meta.actors',
        icon: IconSidebarActors,
      },
      {
        path: '/templates',
        labelKey: 'nav.templates',
        metaKey: 'nav_meta.templates',
        icon: IconSidebarTemplates,
      },
    ],
  },
  {
    id: 'infrastructure',
    labelKey: 'nav_groups.infrastructure',
    entries: [
      {
        path: '/worker-pools',
        labelKey: 'nav.worker_pools',
        metaKey: 'nav_meta.worker_pools',
        icon: IconSidebarWorkerPools,
      },
      {
        path: '/workers',
        labelKey: 'nav.workers',
        metaKey: 'nav_meta.workers',
        icon: IconSidebarWorkers,
      },
      { path: '/substrate-policy', labelKey: 'access.substrate_policy', icon: IconSidebarPolicy },
    ],
  },
  {
    id: 'administration',
    labelKey: 'nav_groups.administration',
    entries: [
      {
        path: '/projects',
        labelKey: 'access.projects',
        metaKey: 'nav_meta.projects',
        icon: IconSidebarProjectList,
      },
      { path: '/users', labelKey: 'access.users', icon: IconSidebarUsers },
    ],
  },
  {
    id: 'gateway',
    labelKey: 'nav_groups.gateway',
    entries: [
      {
        path: '/dashboard',
        labelKey: 'nav.gateway_dashboard',
        metaKey: 'nav_meta.dashboard',
        icon: IconSidebarDashboard,
      },
      {
        path: '/ai-providers',
        labelKey: 'nav.ai_providers',
        metaKey: 'nav_meta.ai_providers',
        icon: IconSidebarProviders,
      },
      {
        path: '/auth-files',
        labelKey: 'nav.auth_files',
        metaKey: 'nav_meta.auth_files',
        icon: IconSidebarAuthFiles,
      },
      { path: '/oauth', labelKey: 'nav.oauth', metaKey: 'nav_meta.oauth', icon: IconSidebarOauth },
      {
        path: '/quota',
        labelKey: 'nav.quota_management',
        metaKey: 'nav_meta.quota_management',
        icon: IconSidebarQuota,
      },
    ],
  },
  {
    id: 'system',
    labelKey: 'nav_groups.system',
    entries: [
      { path: '/logs', labelKey: 'nav.logs', metaKey: 'nav_meta.logs', icon: IconSidebarLogs },
      {
        path: '/config',
        labelKey: 'nav.config_management',
        metaKey: 'nav_meta.config_management',
        icon: IconSidebarConfig,
      },
      ...(plugins
        ? [
            {
              path: '/plugins',
              labelKey: 'nav.plugins',
              metaKey: 'nav_meta.plugins',
              icon: IconSidebarPlugins,
            },
            {
              path: '/plugin-store',
              labelKey: 'nav.plugin_store',
              metaKey: 'nav_meta.plugin_store',
              icon: IconSidebarStore,
            },
          ]
        : []),
      {
        path: '/system',
        labelKey: 'nav.system_info',
        metaKey: 'nav_meta.system_info',
        icon: IconSidebarSystem,
      },
    ],
  },
];

/** Группы меню для роли: пункты, закрытые роли, и опустевшие группы
 * не показываются. Страницы плагинов добавляет MainLayout отдельно.
 * Группа проекта несёт имя выбранного проекта, а «Swarms» -- число роёв,
 * ждущих оператора (#255, правило waitsForOperator -- то же, что у плитки
 * «Ждут вас» обзора, #269); ноль счётчика не рисует. */
export function navSections({
  administrator,
  plugins,
  projectName,
  waitingSwarms = 0,
}: {
  administrator: boolean;
  plugins: boolean;
  projectName?: string;
  waitingSwarms?: number;
}): NavSection[] {
  return sections(plugins)
    .map((section) => ({
      ...section,
      ...(section.id === 'project' && projectName ? { label: projectName } : {}),
      entries: section.entries
        .filter((entry) => visibleAccessPath(entry.path, administrator))
        .map((entry) =>
          entry.path === '/swarms' && waitingSwarms > 0
            ? { ...entry, badge: waitingSwarms, badgeLabelKey: 'nav.waiting_swarms' }
            : entry
        ),
    }))
    .filter((section) => section.entries.length > 0);
}
