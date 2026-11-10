// FEATURE: Navigation grouped by object level (#193).
// SPEC: Groups follow the level of the object: resources of the active
// project, installation infrastructure, administration, the gateway and the
// system. Each object type has its own icon. Members see the same sections as
// before. The work of a swarm lives on its page (#217): no menu entry or link
// leads to the old Workflows and Pipelines addresses, which only redirect.
import { describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { navSections, isNavigationActive } from '@/components/layout/navigation';
import { visibleAccessPath } from '@/features/access/permissions';

const sourceRoot = fileURLToPath(new URL('../src/', import.meta.url));
const source = (path: string) => readFileSync(join(sourceRoot, path), 'utf8');
const locales = ['en', 'ru', 'zh-CN', 'zh-TW', 'ko', 'vi'];
const locale = (name: string) => JSON.parse(source(`i18n/locales/${name}.json`));
const lookup = (tree: unknown, key: string): unknown =>
  key
    .split('.')
    .reduce<unknown>(
      (node, part) =>
        node && typeof node === 'object' ? (node as Record<string, unknown>)[part] : undefined,
      tree
    );

const layout = (administrator: boolean, plugins: boolean) =>
  navSections({ administrator, plugins }).map((section) => [
    section.id,
    section.entries.map((entry) => entry.path),
  ]);

const projectLevel = ['project', ['/project', '/swarms', '/actors', '/templates']];

describe('navigation groups', () => {
  test('an administrator sees every object level in order', () => {
    expect(layout(true, true)).toEqual([
      projectLevel,
      ['infrastructure', ['/worker-pools', '/workers', '/substrate-policy']],
      ['administration', ['/projects', '/users']],
      ['gateway', ['/', '/ai-providers', '/auth-files', '/oauth', '/quota']],
      ['system', ['/logs', '/config', '/plugins', '/plugin-store', '/system']],
    ]);
  });

  test('plugin entries appear only when the backend supports plugins', () => {
    expect(layout(true, false)).toContainEqual(['system', ['/logs', '/config', '/system']]);
  });

  test('a member keeps exactly the former resource sections', () => {
    for (const plugins of [true, false]) {
      expect(layout(false, plugins)).toEqual([projectLevel, ['administration', ['/projects']]]);
    }
  });

  test('every entry has its own icon', () => {
    const entries = navSections({ administrator: true, plugins: true }).flatMap(
      (section) => section.entries
    );
    const markup = entries.map((entry) =>
      renderToStaticMarkup(createElement(entry.icon, { size: 18 }))
    );
    const shared = entries
      .filter((_, index) => markup.indexOf(markup[index]) !== index)
      .map((entry) => entry.path);
    expect(shared).toEqual([]);
  });

  test('every label resolves in every locale', () => {
    const keys = navSections({ administrator: true, plugins: true })
      .flatMap((section) => [
        section.labelKey,
        ...section.entries.flatMap((entry) => [entry.labelKey, entry.metaKey]),
      ])
      .filter((key): key is string => Boolean(key))
      .concat('nav_groups.plugin_pages');
    for (const name of locales) {
      const translations = locale(name);
      const missing = keys.filter((key) => {
        const value = lookup(translations, key);
        return typeof value !== 'string' || value.trim() === '';
      });
      expect([name, missing]).toEqual([name, []]);
    }
  });

  test('every entry opens a route and the layout renders these groups', () => {
    const routes = source('router/MainRoutes.tsx');
    for (const section of navSections({ administrator: true, plugins: true })) {
      for (const entry of section.entries) {
        expect([entry.path, routes.includes(`path: '${entry.path}'`)]).toEqual([entry.path, true]);
      }
    }
    expect(routes).toContain("path: '/project', element: <ProjectDetailPage />");
    expect(source('components/layout/MainLayout.tsx')).toContain('navSections({');
  });
});

describe('active project navigation', () => {
  test('legacy project detail links highlight the current project, not the catalog', () => {
    expect(isNavigationActive('/project', '/projects/project-1', false)).toBe(true);
    expect(isNavigationActive('/projects', '/projects/project-1', true)).toBe(false);
    expect(isNavigationActive('/projects', '/projects', true)).toBe(true);
    expect(isNavigationActive('/project', '/project', true)).toBe(true);
    expect(isNavigationActive('/actors', '/actors', true)).toBe(true);
  });
});

describe('member visibility', () => {
  // Старые адреса прогона открыты участнику, чтобы его ссылка дошла до
  // переадресации на рой, а не упала в список проектов.
  test('the active project and the old run addresses are open to members', () => {
    for (const path of [
      '/project',
      '/workflows',
      '/workflows/runs/:id',
      '/workflows/runs/run-1',
      '/pipelines',
      '/pipelines/:id',
    ]) {
      expect([path, visibleAccessPath(path, false)]).toEqual([path, true]);
    }
  });

  test('infrastructure, users, gateway and system stay admin-only', () => {
    for (const path of [
      '/worker-pools',
      '/workers',
      '/substrate-policy',
      '/users',
      '/projects/p/members',
      '/',
      '/dashboard',
      '/ai-providers',
      '/auth-files',
      '/oauth',
      '/quota',
      '/logs',
      '/config',
      '/plugins',
      '/plugin-store',
      '/system',
      '/projectx',
      '/workflowsx',
    ]) {
      expect([path, visibleAccessPath(path, false)]).toEqual([path, false]);
      expect(visibleAccessPath(path, true)).toBe(true);
    }
  });
});

describe('old run addresses', () => {
  // Ссылки на прогоны разошлись по задачам и комментариям (#217): адреса
  // живут только в таблице переадресаций и в списке открытых участнику.
  test('no internal link points to the Workflows or Pipelines address', () => {
    const files = (dir: string): string[] =>
      readdirSync(dir).flatMap((name) => {
        const path = join(dir, name);
        return statSync(path).isDirectory() ? files(path) : [path];
      });
    const offenders = files(sourceRoot)
      .filter((path) => /\.(ts|tsx)$/.test(path))
      .filter((path) => !path.endsWith(join('router', 'MainRoutes.tsx')))
      .filter((path) => !path.endsWith(join('features', 'access', 'permissions.ts')))
      .filter((path) => /['"`]\/(workflows|pipelines)\b/.test(readFileSync(path, 'utf8')));
    expect(offenders).toEqual([]);
  });
});
