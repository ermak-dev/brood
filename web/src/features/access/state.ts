import type { AccessUser } from '@/services/brood/access';

/** Подпись пользователя в списке выбора: username решает, отображаемое имя
 * лишь помогает узнать человека. */
export function userOption(user: AccessUser) {
  return {
    value: user.username,
    label: user.display_name ? `${user.username} (${user.display_name})` : user.username,
  };
}

/** Кандидаты в участники проекта: активные пользователи установки, которых
 * в проекте ещё нет. Заблокированного добавлять нечего, уже добавленного
 * сервер всё равно отвергнет. */
export function memberCandidates(users: AccessUser[] | null, members: AccessUser[] | null) {
  if (!members) return [];
  const taken = new Set(members.map((member) => member.username));
  return (users ?? [])
    .filter((user) => !user.disabled && !taken.has(user.username))
    .map(userOption);
}

/** Передача доступна только активному участнику, отличному от владельца. */
export function ownerCandidates(members: AccessUser[] | null, owner: string) {
  return (members ?? [])
    .filter((member) => !member.disabled && member.username !== owner)
    .map(userOption);
}

/** Выбранное значение не переживает удаление варианта или ошибку загрузки. */
export function validEditorField(
  field: { type?: string; required?: boolean; options?: ReadonlyArray<{ value: string }> },
  value: string | undefined
) {
  if (field.type === 'select' && value) {
    return Boolean(field.options?.some((option) => option.value === value));
  }
  return !field.required || Boolean(field.type === 'password' ? value : value?.trim());
}

export interface ResourceSnapshot<T> {
  source: () => Promise<T>;
  data: T | null;
  error: string;
  loading: boolean;
}

// При смене проекта прежняя таблица исчезает уже при рендере, до эффекта загрузки.
export function currentResource<T>(snapshot: ResourceSnapshot<T>, source: () => Promise<T>) {
  return snapshot.source === source ? snapshot : { data: null, error: '', loading: true };
}
