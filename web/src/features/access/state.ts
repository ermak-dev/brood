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

/** Открытая панель объекта показывает его свежую строку: после обновления
 * списка -- новые данные, исчезнувший объект закрывает панель. Пока список
 * грузится или не загрузился, остаётся прежняя строка. */
export function retainedRow<T>(
  selected: T | null,
  resource: { data: T[] | null; loading: boolean; error: string },
  same: (row: T, selected: T) => boolean
): T | null {
  if (!selected) return null;
  if (resource.loading || resource.error) return selected;
  return resource.data?.find((row) => same(row, selected)) ?? null;
}

/** Семантические тоны бейджей (#254): attention -- ждёт человека, progress --
 * система работает, problem -- встало, neutral -- ничего не требует.
 * Тон отвечает на вопрос «надо ли смотреть», а не называет состояние: здоровое
 * состояние покоя (active рой, ready роль, answered запрос) не красится.
 * Классы `.tone-<тон>` живут в substrate.module.scss, цвета -- в
 * `--<тон>-badge-*` темы. Один словарь для роёв, исполнения, ролей,
 * запросов и объектов Substrate: #253 и #255 берут его отсюда. */
export const BADGE_TONES = ['attention', 'progress', 'problem', 'neutral'] as const;

export type BadgeTone = (typeof BADGE_TONES)[number];

/** Состояние роя: переходные -- работа системы, error -- проблема. */
export function swarmTone(state: string): BadgeTone {
  if (state === 'provisioning' || state === 'deleting') return 'progress';
  if (state === 'error') return 'problem';
  return 'neutral';
}

/** Исполнение роя (#206): ждущий ответа зовёт оператора, остановленный --
 * нет; они не делят тон. Незнакомое состояние ничего не требует. */
export function executionTone(execution: string): BadgeTone {
  switch (execution) {
    case 'waiting':
      return 'attention';
    case 'running':
      return 'progress';
    case 'failed':
      return 'problem';
    default:
      return 'neutral';
  }
}

const brokenRoleStates = new Set(['stale', 'inactive', 'foreign', 'missing']);

/** Роль роя (#241): непривязанную должен привязать человек, сломанная
 * привязка -- проблема, готовая ничего не требует. Незнакомое состояние
 * (сервер новее панели) не выдаётся за другое. */
export function roleTone(state: string): BadgeTone {
  if (state === 'unbound') return 'attention';
  return brokenRoleStates.has(state) ? 'problem' : 'neutral';
}

/** Запрос оператору (#216): открытый ждёт человека, остальное -- история. */
export function requestTone(state: string): BadgeTone {
  return state === 'open' ? 'attention' : 'neutral';
}

/** Доставка ответа актору: идёт -- работа, провал -- проблема. */
export function deliveryTone(delivery: string): BadgeTone {
  if (delivery === 'delivered') return 'neutral';
  if (delivery === 'failed') return 'problem';
  return 'progress';
}

const substrateTones: Record<string, BadgeTone> = {
  running: 'progress',
  active: 'progress',
  resuming: 'progress',
  suspending: 'progress',
  reverting: 'progress',
  draining: 'progress',
  deleting: 'progress',
  suspended: 'neutral',
  unavailable: 'problem',
  crashed: 'problem',
};

/** Актор или воркер Substrate по короткому имени состояния: работающий и
 * переходный -- работа, упавший и недоступный -- проблема. */
export function substrateTone(state: string): BadgeTone {
  return substrateTones[state] ?? 'neutral';
}
