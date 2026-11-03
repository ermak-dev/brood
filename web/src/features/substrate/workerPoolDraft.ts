import type { WorkerPool, WorkerPoolDefaults } from '@/services/brood/substrate';
import { clone, isLabelKey, isLabelValue } from './format';

export interface PoolDraft {
  name: string;
  replicas: string;
  workerImage: string;
  sandboxClass: string;
  requestCpu: string;
  requestMemory: string;
  limitCpu: string;
  limitMemory: string;
  // Метки пула, а не его подов: планировщик читает их с записи Worker, куда
  // ate-controller переносит именно metadata.labels. Без них пул не совпадёт
  // ни с одним workerSelector.
  labels: Record<string, string>;
}

/** Классы песочницы, которые понимает панель, в порядке предложения. Один
 * список на форму и проверку: иначе они разъедутся. */
export const SANDBOX_CLASSES = ['gvisor', 'microvm', 'lxc'] as const;

const declaredImages = (defaults: WorkerPoolDefaults): Record<string, string> =>
  defaults.workerImages ?? { gvisor: defaults.workerImage };

export function workerPoolImages(defaults: WorkerPoolDefaults): Record<string, string> {
  const images = declaredImages(defaults);
  return Object.fromEntries(SANDBOX_CLASSES
    .filter((name) => Boolean(images[name]))
    .map((name) => [name, images[name]]));
}

/** Поддерживаемые классы, для которых установка не объявила образ. Панель
 * называет их вместо того, чтобы молча опустить: иначе неустановленный класс
 * неотличим от неподдерживаемого. */
export function missingSandboxClasses(defaults: WorkerPoolDefaults): string[] {
  const images = declaredImages(defaults);
  return SANDBOX_CLASSES.filter((name) => !images[name]);
}

/** Прочитанный пул несёт предусловия uid и resourceVersion: без них ни правка,
 * ни удаление не сверятся с тем, что видел пользователь. */
export const versioned = (pool: WorkerPool) => Boolean(pool.metadata.uid && pool.metadata.resourceVersion);

export function newPoolDraft(workerImage: string, sandboxClass = 'gvisor'): PoolDraft {
  return {
    name: '', replicas: '1', workerImage, sandboxClass,
    requestCpu: '', requestMemory: '', limitCpu: '', limitMemory: '', labels: {},
  };
}

export function poolDraft(pool: WorkerPool): PoolDraft {
  const resources = pool.spec.template?.resources;
  return {
    name: pool.metadata.name,
    replicas: String(pool.spec.replicas),
    workerImage: pool.spec.workerImage,
    sandboxClass: pool.spec.sandboxClasses[0]?.name ?? '',
    requestCpu: resources?.requests?.cpu ?? '',
    requestMemory: resources?.requests?.memory ?? '',
    limitCpu: resources?.limits?.cpu ?? '',
    limitMemory: resources?.limits?.memory ?? '',
    labels: { ...(pool.metadata.labels ?? {}) },
  };
}

// Kubernetes Quantity: DecimalSI, BinarySI или DecimalExponent; отрицательные
// ресурсы не допустимы. Сервер проверяет точность и остальные ограничения.
function quantity(value: string): number | null {
  const match = /^(\+?(?:\d+(?:\.\d*)?|\.\d+))([eE][+-]?\d+|[numkMGTPE]|[KMGTPE]i)?$/.exec(value);
  if (!match) return null;
  const suffix = match[2] ?? '';
  const powers: Record<string, number> = { n: -9, u: -6, m: -3, k: 3, M: 6, G: 9, T: 12, P: 15, E: 18 };
  const exponent = /^[eE]/.test(suffix) && suffix.length > 1
    ? Number(suffix.slice(1)) : (powers[suffix] ?? 0);
  // Одно десятичное преобразование: 700 * 0.001 уже больше 0.7 в float.
  const result = suffix.endsWith('i')
    ? Number(match[1]) * 1024 ** ('KMGTPE'.indexOf(suffix[0]) + 1)
    : Number(`${match[1]}e${exponent}`);
  return Number.isFinite(result) ? result : null;
}

export function validatePoolDraft(draft: PoolDraft): Partial<Record<keyof PoolDraft, string>> {
  const errors: Partial<Record<keyof PoolDraft, string>> = {};
  if (draft.name.length > 253 || !draft.name.split('.').every(
    (label) => label.length <= 63 && /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(label)
  )) errors.name = 'name';
  if (!/^\d+$/.test(draft.replicas) || Number(draft.replicas) > 2147483647) errors.replicas = 'replicas';
  if (!draft.workerImage || /\s/.test(draft.workerImage)) errors.workerImage = 'image';
  if (!SANDBOX_CLASSES.includes(draft.sandboxClass as (typeof SANDBOX_CLASSES)[number]))
    errors.sandboxClass = 'image';
  if (!Object.entries(draft.labels).every(([key, value]) => isLabelKey(key) && isLabelValue(value)))
    errors.labels = 'label';
  for (const key of ['requestCpu', 'requestMemory', 'limitCpu', 'limitMemory'] as const) {
    if (draft[key] !== '' && quantity(draft[key]) === null) errors[key] = 'quantity';
  }
  for (const [request, limit] of [['requestCpu', 'limitCpu'], ['requestMemory', 'limitMemory']] as const) {
    if (draft[request] && draft[limit] && !errors[request] && !errors[limit] &&
        quantity(draft[request])! > quantity(draft[limit])!) errors[limit] = 'limit';
  }
  return errors;
}

// Меняем только поля формы; неизвестные ресурсы, размещение и предусловия
// остаются от прочитанного объекта. Пустая строка снимает явный ресурс.
export function poolPayload(source: WorkerPool | null, draft: PoolDraft): WorkerPool {
  const pool: WorkerPool = source ? clone(source) : {
    metadata: { name: draft.name },
    spec: { replicas: 1, workerImage: draft.workerImage, sandboxClasses: [{ name: draft.sandboxClass }] },
  };
  delete pool.status;
  if (Object.keys(draft.labels).length) pool.metadata.labels = { ...draft.labels };
  else delete pool.metadata.labels;
  pool.spec.replicas = Number(draft.replicas);
  for (const [group, resource, field] of [
    ['requests', 'cpu', 'requestCpu'], ['requests', 'memory', 'requestMemory'],
    ['limits', 'cpu', 'limitCpu'], ['limits', 'memory', 'limitMemory'],
  ] as const) {
    if (draft[field] !== '') {
      pool.spec.template ??= {};
      pool.spec.template.resources ??= {};
      pool.spec.template.resources[group] ??= {};
      pool.spec.template.resources[group][resource] = draft[field];
    } else {
      const values = pool.spec.template?.resources?.[group];
      if (values) delete values[resource];
    }
  }
  return pool;
}
