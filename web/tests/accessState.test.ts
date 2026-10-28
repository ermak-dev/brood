// FEATURE: Безопасный выбор проекта независимо от выбранного Substrate-роя.
// SPEC: Неизвестный ID не переключает контекст на первый проект; устаревшие ответы скрыты.
// IMPLEMENTATION: Чистые функции выбора проекта и видимости результата загрузки.
// RESULT: IMPLEMENTED — пустые коллекции и смена контекста проверены.
// STATUS: DONE
import { expect, test } from 'bun:test';

// Имя пользователя в правах выбирается из списка: кандидат в участники -- это
// активный пользователь установки, которого в проекте ещё нет.
test('member candidates exclude existing members and disabled users', async () => {
  const { memberCandidates, userOption } = await import('../src/features/access/state');
  const users = [
    { username: 'alice', display_name: 'Alice', is_admin: false, disabled: false },
    { username: 'bob', display_name: '', is_admin: false, disabled: false },
    { username: 'carol', display_name: 'Carol', is_admin: false, disabled: true },
  ];
  const members = [{ username: 'alice', display_name: 'Alice', is_admin: false, disabled: false }];
  expect(memberCandidates(users, members)).toEqual([{ value: 'bob', label: 'bob' }]);
  expect(memberCandidates(users, [])).toEqual([
    { value: 'alice', label: 'alice (Alice)' },
    { value: 'bob', label: 'bob' },
  ]);
  expect(memberCandidates(null, null)).toEqual([]);
  expect(memberCandidates(users, null)).toEqual([]);
  expect(userOption(users[0])).toEqual({ value: 'alice', label: 'alice (Alice)' });
});

test('new owners are active members other than the current owner', async () => {
  const { ownerCandidates } = await import('../src/features/access/state');
  const member = (username: string, disabled = false) => ({
    username,
    display_name: '',
    is_admin: false,
    disabled,
  });
  expect(ownerCandidates([member('alice'), member('bob'), member('carol', true)], 'alice')).toEqual(
    [{ value: 'bob', label: 'bob' }]
  );
  expect(ownerCandidates(null, 'alice')).toEqual([]);
});

test('select values must still belong to the current options', async () => {
  const { validEditorField } = await import('../src/features/access/state');
  const field = { type: 'select' as const, required: true, options: [{ value: 'alice' }] };
  expect(validEditorField(field, 'alice')).toBe(true);
  expect(validEditorField(field, 'bob')).toBe(false);
  expect(validEditorField({ ...field, options: [] }, 'alice')).toBe(false);
  expect(validEditorField(field, '')).toBe(false);
  expect(validEditorField({ type: 'password', required: true }, '  ')).toBe(true);
  expect(validEditorField({ required: true }, '  ')).toBe(false);
});

// Проект выбирается маршрутом: прежние ссылки с параметром запроса ведут в
// раздел того же проекта, а без параметра -- к списку проектов.
test('legacy project links map to the project section route', async () => {
  const { projectSectionPath } = await import('../src/features/access/state');
  expect(projectSectionPath('swarms', 'project-1')).toBe('/projects/project-1/swarms');
  expect(projectSectionPath('members', 'project-1')).toBe('/projects/project-1/members');
  expect(projectSectionPath('swarms', 'team/one')).toBe('/projects/team%2Fone/swarms');
  expect(projectSectionPath('swarms', null)).toBe('/projects');
  expect(projectSectionPath('members', '')).toBe('/projects');
});

test('selects the first project only without an explicit project ID', async () => {
  const { selectAccessProject, currentResource } = await import('../src/features/access/state');
  const projects = [{ id: 'project-1', name: 'Team' }];
  expect(selectAccessProject(projects, null)).toEqual(projects[0]);
  expect(selectAccessProject(projects, 'project-1')).toEqual(projects[0]);
  expect(selectAccessProject(projects, 'swarm-id')).toBeUndefined();
  expect(selectAccessProject([], null)).toBeUndefined();
  expect(selectAccessProject(null, 'project-1')).toBeUndefined();
  const oldLoad = async () => ['old'];
  const newLoad = async () => ['new'];
  const old = { source: oldLoad, data: ['old'], error: '', loading: false };
  expect(currentResource(old, newLoad)).toEqual({ data: null, error: '', loading: true });
  expect(currentResource(old, oldLoad)).toMatchObject({ data: ['old'], loading: false });
});
