// FEATURE: Безопасный выбор проекта независимо от выбранного Substrate-роя.
// SPEC: Неизвестный ID не переключает контекст на первый проект; устаревшие ответы скрыты.
// IMPLEMENTATION: Чистые функции выбора проекта и видимости результата загрузки.
// RESULT: IMPLEMENTED — пустые коллекции и смена контекста проверены.
// STATUS: DONE
import { expect, test } from 'bun:test';

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
