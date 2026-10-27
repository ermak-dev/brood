// FEATURE: Расширение исходной панели сущностями brood без замены старых страниц.
// SPEC: Админские разделы закрыты, проект не является роем, все шесть локалей полны.
// IMPLEMENTATION: Новые маршруты, фильтр навигации, явные ограничения API.
// RESULT: IMPLEMENTED — исходные разделы сохранены, права и локали проверены.
// STATUS: DONE
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');

test('retains original routes and adds isolated access routes with admin guards', () => {
  const routes = source('router/MainRoutes.tsx');
  for (const path of [
    '/projects',
    '/swarms',
    '/users',
    '/permissions',
    '/actors',
    '/templates',
    '/ai-providers',
    '/auth-files',
  ]) {
    expect(routes).toContain(`path: '${path}'`);
  }
  expect(routes).toContain('AccessProjectsPage');
  expect(routes).toContain('isSuperuser');
  expect(routes).toContain('AdminOnly');
  expect(source('components/layout/MainLayout.tsx')).toContain('visibleAccessPath');
});

test('membership and retry capabilities are stated truthfully, never guessed', async () => {
  const { accessCapabilities, visibleAccessPath } =
    await import('../src/features/access/permissions');
  expect(accessCapabilities).toEqual({ listMembers: false, retrySwarm: false });
  for (const path of [
    '/users',
    '/workers',
    '/worker-pools',
    '/policy',
    '/config',
    '/ai-providers',
  ]) {
    expect(visibleAccessPath(path, false)).toBe(false);
    expect(visibleAccessPath(path, true)).toBe(true);
  }
  for (const path of ['/projects', '/swarms', '/permissions', '/actors', '/templates']) {
    expect(visibleAccessPath(path, false)).toBe(true);
  }
});

test('all locales explain actual permissions and unavailable capabilities', () => {
  for (const locale of ['en', 'ru', 'zh-CN', 'zh-TW', 'ko', 'vi']) {
    const access = JSON.parse(source(`i18n/locales/${locale}.json`)).access;
    for (const key of [
      'projects',
      'swarms',
      'users',
      'permissions',
      'member_policy',
      'admin_policy',
      'members_unavailable',
      'retry_unavailable',
      'transfer_hint',
    ]) {
      expect(typeof access?.[key]).toBe('string');
      expect(access[key].length).toBeGreaterThan(3);
    }
  }
});
