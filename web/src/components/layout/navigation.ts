import type { ComponentType } from 'react';
import {
  IconSidebarAgents,
  IconSidebarAuthFiles,
  IconSidebarConfig,
  IconSidebarDashboard,
  IconSidebarLogs,
  IconSidebarOauth,
  IconSidebarPlugins,
  IconSidebarPools,
  IconSidebarProjects,
  IconSidebarProviders,
  IconSidebarQuota,
  IconSidebarStore,
  IconSidebarSwarms,
  IconSidebarSystem,
  IconSidebarUsers,
  type IconProps,
} from '@/components/ui/icons';
import { visiblePath } from '@/features/brood/routes';

export interface NavEntry {
  path: string;
  labelKey: string;
  metaKey?: string;
  icon: ComponentType<IconProps>;
}

export interface NavSection {
  id: 'brood' | 'gateway' | 'system';
  labelKey: string;
  entries: NavEntry[];
}

// Every brood entity is a menu entry with a flat table of its own.
// visiblePath hides the administrator's entries from everyone else; the
// server checks each request.
const sections = (plugins: boolean): NavSection[] => [
  {
    id: 'brood',
    labelKey: 'nav_groups.brood',
    entries: [
      { path: '/projects', labelKey: 'nav.projects', icon: IconSidebarProjects },
      { path: '/swarms', labelKey: 'nav.swarms', icon: IconSidebarSwarms },
      { path: '/agents', labelKey: 'nav.agents', icon: IconSidebarAgents },
      { path: '/pools', labelKey: 'nav.pools', icon: IconSidebarPools },
      { path: '/users', labelKey: 'nav.users', icon: IconSidebarUsers },
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

/** Menu groups for the role: entries closed to it and emptied groups are not
 * shown. MainLayout adds plugin pages separately. */
export function navSections({
  administrator,
  plugins,
}: {
  administrator: boolean;
  plugins: boolean;
}): NavSection[] {
  return sections(plugins)
    .map((section) => ({
      ...section,
      entries: section.entries.filter((entry) => visiblePath(entry.path, administrator)),
    }))
    .filter((section) => section.entries.length > 0);
}
