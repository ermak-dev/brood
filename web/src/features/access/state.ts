import type { AccessProject } from '@/services/brood/access';

export function selectAccessProject(projects: AccessProject[] | null, requested: string | null) {
  return requested === null ? projects?.[0] : projects?.find((row) => row.id === requested);
}

/** Раздел проекта в маршруте. Старая ссылка выбирала проект параметром
 * запроса; без параметра адресовать нечего, и ведём к списку проектов. */
export function projectSectionPath(section: 'swarms' | 'members', project: string | null) {
  return project ? `/projects/${encodeURIComponent(project)}/${section}` : '/projects';
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
