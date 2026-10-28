// FEATURE: Расширение исходной панели сущностями brood без замены старых страниц.
// SPEC: Админские разделы закрыты, рой не дублируется областью, локали полны.
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

// Рой и его atespace — одна сущность интерфейса: второй страницы областей
// нет, старый адрес ведёт к проектам (docs/MODEL.md).
test('the substrate areas page is a redirect to projects, not a second page', () => {
  const routes = source('router/MainRoutes.tsx');
  expect(routes).toContain(
    `path: '/substrate-projects', element: <Navigate to="/projects" replace />`
  );
  expect(routes).not.toContain('substrate/ProjectsPage');
  const layout = source('components/layout/MainLayout.tsx');
  expect(layout).not.toContain('/substrate-projects');
  expect(layout).not.toContain('access.substrate_projects');
});

// Выбор проекта живёт в маршруте, а не во втором выпадающем списке рядом с
// глобальным выбором роя; старые ссылки ведут в раздел того же проекта.
test('a project is chosen by the route, and swarms and members are its sections', () => {
  const routes = source('router/MainRoutes.tsx');
  expect(routes).toContain(`path: '/projects/:project/swarms'`);
  expect(routes).toContain(`path: '/projects/:project/members'`);
  expect(routes).toContain('LegacyProjectRoute section="swarms"');
  expect(routes).toContain('LegacyProjectRoute section="members"');
  expect(routes).toContain('projectSectionPath(section, params.get(\'project\'))');
  expect(routes).not.toContain('ProjectSelect');
  for (const page of ['features/access/SwarmsPage.tsx', 'features/access/PermissionsPage.tsx']) {
    expect(source(page)).toContain('ProjectRoute');
    expect(source(page)).not.toContain('ProjectSelect');
  }
  // Рои и участники -- разделы проекта, а не пункты меню рядом с ним.
  const layout = source('components/layout/MainLayout.tsx');
  expect(layout).not.toContain(`path: '/swarms'`);
  expect(layout).not.toContain(`path: '/permissions'`);
  expect(layout).toContain(`path: '/projects'`);
  // Участник проходит во вложенный маршрут, админское остаётся закрытым.
  expect(source('features/access/permissions.ts')).toContain('startsWith');
});

test('membership and retry capabilities are stated truthfully, never guessed', async () => {
  const { visibleAccessPath } = await import('../src/features/access/permissions');
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
  for (const path of [
    '/projects',
    '/projects/:project/swarms',
    '/projects/:project/members',
    '/swarms',
    '/permissions',
    '/actors',
    '/templates',
  ]) {
    expect(visibleAccessPath(path, false)).toBe(true);
  }
  // Список участников существует и читается только администратором: панель
  // не запрашивает его обычному пользователю и не утверждает обратного.
  const page = source('features/access/PermissionsPage.tsx');
  expect(page).toContain('admin ? accessApi.members(projectId)');
});

test('all locales explain actual permissions without dead claims', () => {
  for (const locale of ['en', 'ru', 'zh-CN', 'zh-TW', 'ko', 'vi']) {
    const translations = JSON.parse(source(`i18n/locales/${locale}.json`));
    const access = translations.access;
    for (const key of [
      'projects',
      'swarms',
      'users',
      'permissions',
      'member_policy',
      'admin_policy',
      'retry_unavailable',
      'transfer_hint',
    ]) {
      expect(typeof access?.[key]).toBe('string');
      expect(access[key].length).toBeGreaterThan(3);
    }
    // Удалённые утверждения не возвращаются: списка участников нет в API —
    // неправда, а второй страницы областей больше не существует.
    expect(access?.members_unavailable).toBeUndefined();
    expect(access?.substrate_projects).toBeUndefined();
    expect(translations.projects).toBeUndefined();
    // Подзаголовок пункта проектов — короткая строка, а не абзац политики.
    const meta = translations.nav_meta?.projects;
    expect(typeof meta).toBe('string');
    expect(meta.length).toBeLessThan(40);
    expect(meta.toLowerCase()).not.toContain('atespace');
  }
});
