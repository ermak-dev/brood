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

// Открытая панель объекта (#157) показывает свежую строку списка: после
// обновления -- новые данные, исчезнувший объект закрывает панель, а пока
// список грузится или не загрузился, прежняя строка остаётся на месте.
test('an open panel follows its row through list refreshes', async () => {
  const { retainedRow } = await import('../src/features/access/state');
  const same = (a: { id: string }, b: { id: string }) => a.id === b.id;
  const open = { id: 'a', name: 'old' };
  const loaded = (data: Array<{ id: string; name: string }>) => ({
    data,
    loading: false,
    error: '',
  });
  expect(retainedRow(null, loaded([open]), same)).toBeNull();
  expect(retainedRow(open, loaded([{ id: 'a', name: 'new' }]), same)).toEqual({
    id: 'a',
    name: 'new',
  });
  expect(retainedRow(open, loaded([{ id: 'b', name: 'other' }]), same)).toBeNull();
  expect(retainedRow(open, { data: null, loading: true, error: '' }, same)).toBe(open);
  expect(retainedRow(open, { data: null, loading: false, error: 'refused' }, same)).toBe(open);
});

// Состояние роя в панели -- бейдж с тоном: сбой виден как сбой, переходное
// как ожидание, а не всё одинаково серым.
test('swarm state badges distinguish failure from transition', async () => {
  const { swarmTone } = await import('../src/features/access/state');
  expect(swarmTone('active')).toBe('success');
  expect(swarmTone('provisioning')).toBe('warning');
  expect(swarmTone('deleting')).toBe('warning');
  expect(swarmTone('error')).toBe('danger');
  expect(swarmTone('deleted')).toBe('muted');
  expect(swarmTone('unexpected')).toBe('muted');
});

// Выбор теперь явный и глобальный; функция автоматического выбора первого
// проекта удалена. Защита от ответа предыдущего контекста остаётся.
test('never exposes a result from the previous project loader', async () => {
  const { currentResource } = await import('../src/features/access/state');
  const oldLoad = async () => ['old'];
  const newLoad = async () => ['new'];
  const old = { source: oldLoad, data: ['old'], error: '', loading: false };
  expect(currentResource(old, newLoad)).toEqual({ data: null, error: '', loading: true });
  expect(currentResource(old, oldLoad)).toMatchObject({ data: ['old'], loading: false });
});
