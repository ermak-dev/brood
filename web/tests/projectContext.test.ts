// FEATURE: A project is the only global context; swarms are not globally selected.
// SPEC: Admins see every section; ordinary users see only the project overview at the
// root (#197), projects, swarms, templates and actors.
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { visibleAccessPath } from '@/features/access/permissions';
const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');

test('ordinary navigation is exactly the requested four resource sections', () => {
  for (const path of ['/', '/projects', '/swarms', '/actors', '/templates']) {
    expect(visibleAccessPath(path, false)).toBe(true);
    expect(visibleAccessPath(path, true)).toBe(true);
  }
  for (const path of [
    '/dashboard',
    '/policy',
    '/users',
    '/permissions',
    '/projects/id/members',
    '/workers',
    '/worker-pools',
    '/ai-providers',
    '/auth-files',
    '/oauth',
    '/quota',
    '/logs',
    '/config',
    '/system',
  ]) {
    expect(visibleAccessPath(path, false)).toBe(false);
    expect(visibleAccessPath(path, true)).toBe(true);
  }
});

test('global switcher contains projects only, with no Projects shortcut', () => {
  const switcher = source('features/substrate/components/ProjectSwitcher.tsx');
  expect(switcher).toMatch(/accessApi\s*\.\s*projects\(/);
  expect(switcher).not.toContain('scopesApi');
  expect(switcher).not.toContain("navigate('/projects')");
  expect(switcher).not.toContain('access.projects');
  // #194: the address carries the selection of the old project, which the switch drops.
  expect(switcher).toContain('projectSelectionPath(pathname, item.id, search)');
  expect(switcher).toContain('if (destination) navigate(destination)');
  expect(switcher).not.toContain('role="menuitem"');
  expect(source('stores/useProjectStore.ts')).toContain("name: 'brood-project'");
  expect(source('features/access/ProjectsPage.tsx')).toContain('access.choose_project');
});

test('actors and templates enumerate accessible swarms without setting a global swarm', () => {
  for (const page of ['ActorsPage', 'TemplatesPage']) {
    expect(source(`features/substrate/${page}.tsx`)).toContain('SwarmSections');
  }
  const swarms = source('features/access/SwarmsPage.tsx');
  expect(swarms).not.toContain('setSwarm(');
  expect(swarms).not.toContain('activeSwarm');
});

test('header refresh supports every resource group in a project', async () => {
  const { createRefreshRegistry } = await import('../src/hooks/useHeaderRefresh');
  const registry = createRefreshRegistry();
  const called: string[] = [];
  const offFirst = registry.subscribe(async () => {
    called.push('one');
  });
  const offSecond = registry.subscribe(async () => {
    called.push('two');
  });
  await registry.refresh();
  expect(called.sort()).toEqual(['one', 'two']);
  called.length = 0;
  offFirst();
  await registry.refresh();
  expect(called).toEqual(['two']);
  offSecond();
  called.length = 0;
  await registry.refresh();
  expect(called).toEqual([]);
});
