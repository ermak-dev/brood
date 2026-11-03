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

// Старые ссылки сохраняют выбор проекта в новом глобальном контексте;
// второй независимый селектор не возвращается.
test('legacy project routes bind the global project before opening a section', () => {
  const routes = source('router/MainRoutes.tsx');
  expect(routes).toContain(`path: '/projects/:project/swarms'`);
  expect(routes).toContain(`path: '/projects/:project/members'`);
  expect(routes).toContain('LegacyProjectRoute section="swarms"');
  expect(routes).toContain('LegacyProjectRoute section="members"');
  expect(routes).toContain("route.project || params.get('project')");
  expect(routes).toContain('setProject(requested)');
  expect(routes).not.toContain('ProjectSelect');
  for (const page of ['features/access/SwarmsPage.tsx', 'features/access/ProjectDetailPage.tsx']) {
    expect(source(page)).toContain('ProjectScope');
    expect(source(page)).not.toContain('ProjectSelect');
  }
  // Каталог роёв — самостоятельный ресурс выбранного проекта; управление
  // участниками остаётся администраторским действием.
  const layout = source('components/layout/MainLayout.tsx');
  expect(layout).toContain(`path: '/swarms'`);
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
    '/permissions',
    '/projects/:project/members',
    '/config',
    '/ai-providers',
  ]) {
    expect(visibleAccessPath(path, false)).toBe(false);
    expect(visibleAccessPath(path, true)).toBe(true);
  }
  for (const path of [
    '/projects',
    '/projects/:project/swarms',
    '/swarms',
    '/actors',
    '/templates',
  ]) {
    expect(visibleAccessPath(path, false)).toBe(true);
  }
  // Участники проекта читают состав, но не получают админские действия:
  // действия панели участника решает memberPlan по признаку администратора
  // (memberPlan(false) пуст -- accessPanels.test.ts).
  const page = source('features/access/PermissionsPage.tsx');
  expect(page).toContain('() => accessApi.members(projectId)');
  expect(page).toContain('memberPlan(admin)');
  expect(page).toContain('adding && admin');
  expect(page).toContain('admin && adding ? accessApi.users()');
});

/** JSX каждого <EntityTable ... /> страницы: до "/>" вне фигурных скобок,
 * чтобы разметка внутри render-функций колонок не обрывала поиск. */
function entityTables(text: string): string[] {
  const tables: string[] = [];
  for (
    let start = text.indexOf('<EntityTable');
    start >= 0;
    start = text.indexOf('<EntityTable', start + 1)
  ) {
    let depth = 0;
    let end = start;
    for (; end < text.length; end += 1) {
      if (text[end] === '{') depth += 1;
      if (text[end] === '}') depth -= 1;
      if (depth === 0 && text.startsWith('/>', end)) break;
    }
    tables.push(text.slice(start, end + 2));
  }
  return tables;
}

// #157: строка таблицы страницы только открывает панель объекта; кнопок в
// строке нет, все действия -- в панели, по плану с теми же правами.
test('page tables open an object panel instead of carrying row buttons', () => {
  const pages = {
    ProjectsPage: 'projectPlan(admin)',
    SwarmsPage: 'swarmPlan(swarm, admin)',
    UsersPage: 'userPlan()',
    PermissionsPage: 'memberPlan(admin)',
  };
  for (const [page, plan] of Object.entries(pages)) {
    const text = source(`features/access/${page}.tsx`);
    const tables = entityTables(text);
    expect(tables.length).toBe(1);
    for (const table of tables) {
      expect(table).toContain('onRowClick=');
      expect(table).not.toContain('actions=');
      expect(table).not.toContain('actionsLabel=');
    }
    expect(text).toContain('<EntityPanel');
    expect(text).toContain(`panelGroups(${plan}`);
  }
  // Таблица внутри листа доступа к рою -- вложенная, её строки сохраняют
  // свои кнопки (EntityTable: actions только для таблиц внутри панели).
  expect(entityTables(source('features/access/SwarmAccess.tsx'))[0]).toContain('actions=');
});

// Перенос в панель не меняет подтверждений: те же заголовок, текст и
// последствие, что у прежней кнопки строки.
test('panel actions keep the confirmations of the former row buttons', () => {
  const projects = source('features/access/ProjectsPage.tsx');
  expect(projects).toContain("t('access.confirm_project', { name: project.name })");
  expect(projects).toContain('await accessApi.deleteProject(project.id)');
  expect(projects).toContain("if (selected === project.id) setProject('')");
  const users = source('features/access/UsersPage.tsx');
  expect(users).toContain("t('access.confirm_user', { name: user.username })");
  expect(users).toContain('await useAuthStore.getState().checkAuth()');
  const members = source('features/access/PermissionsPage.tsx');
  expect(members).toContain("t('access.confirm_member', { name: member.username })");
  expect(members).toContain('accessApi.removeMember(projectId, member.username)');
  const swarms = source('features/access/SwarmsPage.tsx');
  expect(swarms).toContain('confirm(swarm.name, async () => {');
  expect(swarms).toContain('await accessApi.deleteSwarm(swarm.id)');
  // Повтор создания идёт без вопроса, как и прежде: он продолжает
  // сохранённую операцию, а не разрушает.
  expect(swarms).toContain('run(() => accessApi.createSwarm(swarm.project_id, swarm.name))');
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
      'retry_hint',
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

// Управление доступом не вводит имена руками: username приходит из списка, а
// исключение участника живёт в его строке.
test('access editors pick a user from a list instead of typing a username', () => {
  const members = source('features/access/PermissionsPage.tsx');
  expect(members).toContain("type: 'select'");
  expect(members).toContain('memberCandidates(users.data, members.data)');
  // Второго, текстового способа исключить участника больше нет.
  expect(members).not.toContain("setEditor('remove')");
  const swarms = source('features/access/SwarmsPage.tsx');
  expect(swarms).toContain("type: 'select'");
  expect(swarms).toContain('options: owners');
  for (const page of [members, swarms]) {
    expect(page).not.toContain('values.username.trim()');
  }
  // Поле выбора умеет пустой список: он объясняется, а не выглядит поломкой.
  const editor = source('features/access/shared.tsx');
  expect(editor).toContain('field.options?.length');
  expect(editor).toContain('field.empty');
});

test('project sections register one refresh for tables and picker resources', () => {
  for (const page of ['PermissionsPage', 'SwarmsPage']) {
    const text = source(`features/access/${page}.tsx`);
    expect(text).toContain('useHeaderRefresh(');
    expect(text.match(/headerRefresh: false/g)?.length).toBe(2);
    expect(text).toContain('Promise.all(');
  }
  expect(source('features/substrate/components/ProjectScope.tsx')).toContain(
    'headerRefresh: false'
  );
});

// Подтверждение называет последствие своего действия, а не чужого.
test('each confirmed action carries its own consequence', () => {
  expect(source('features/access/ProjectsPage.tsx')).toContain('access.confirm_project');
  expect(source('features/access/PermissionsPage.tsx')).toContain('access.confirm_member');
  expect(source('features/access/UsersPage.tsx')).toContain('access.confirm_user');
  for (const locale of ['en', 'ru', 'zh-CN', 'zh-TW', 'ko', 'vi']) {
    const access = JSON.parse(source(`i18n/locales/${locale}.json`)).access;
    for (const key of ['confirm', 'confirm_project', 'confirm_member']) {
      expect(typeof access[key]).toBe('string');
      expect(access[key]).toContain('{{name}}');
    }
    for (const key of ['no_candidates', 'no_members']) {
      expect(access[key].length).toBeGreaterThan(3);
    }
    // Нейтральный текст больше не обещает удаления проекта при любом действии.
    expect(access.confirm.length).toBeLessThan(access.confirm_project.length);
  }
});
