/**
 * Исполнение и открытые вопросы роёв в списке (#255). Сервер пока не несёт
 * сводку в строке списка (#234), поэтому панель собирает её сама по старым
 * маршрутам: прогон роя (`GET /api/pipeline-runs/{id}`, #206) и его запросы
 * оператору (`GET /api/swarms/{id}/requests`, #216) -- только для активных
 * роёв и не больше ACTIVITY_LIMIT. Когда строка списка принесёт `execution`
 * и `openRequests`, опрос этого роя не нужен (swarmActivityOf).
 */
import type { AccessSwarm } from '@/services/brood/access';
import type { BadgeTone } from './state';

export interface SwarmActivity {
  /** Исполнение в словах страницы роя: idle, running, waiting, succeeded,
   * failed, stopped; незнакомое значение остаётся как есть. */
  execution: string;
  openRequests: number;
}

/** Сколько роёв панель опрашивает сама: по два запроса на рой. */
export const ACTIVITY_LIMIT = 20;

/** Сводка из строки списка (#234), если сервер уже отдаёт её. */
export function swarmActivityOf(swarm: AccessSwarm): SwarmActivity | undefined {
  if (typeof swarm.execution !== 'string' || !Number.isSafeInteger(swarm.openRequests)) {
    return undefined;
  }
  return { execution: swarm.execution, openRequests: swarm.openRequests as number };
}

/** Рои, которые панель опрашивает сама: активные, без сводки сервера,
 * первые ACTIVITY_LIMIT в порядке списка. */
export function activityTargets(swarms: AccessSwarm[], limit = ACTIVITY_LIMIT): AccessSwarm[] {
  return swarms
    .filter((swarm) => swarm.state === 'active' && swarmActivityOf(swarm) === undefined)
    .slice(0, limit);
}

/** Исполнение по проводу прогона: без графа (404 -> null) рой не запускался,
 * waiting_human -- это waiting (как swarmExecution страницы роя). */
export function executionOfRun(run: { status: string } | null): string {
  if (!run) return 'idle';
  return run.status === 'waiting_human' ? 'waiting' : run.status;
}

export interface ActivitySource {
  run: (swarmId: string) => Promise<{ status: string } | null>;
  requests: (swarmId: string) => Promise<Array<{ state: string }>>;
}

/** Сводка каждого роя: оба запроса параллельно, отказ любого из них даёт
 * null -- «неизвестно», а не ноль вопросов и не «не запущен». */
export async function loadSwarmActivities(
  swarms: AccessSwarm[],
  source: ActivitySource
): Promise<Record<string, SwarmActivity | null>> {
  const entries = await Promise.all(
    swarms.map(async (swarm): Promise<[string, SwarmActivity | null]> => {
      try {
        const [run, requests] = await Promise.all([
          source.run(swarm.id),
          source.requests(swarm.id),
        ]);
        return [
          swarm.id,
          {
            execution: executionOfRun(run),
            openRequests: requests.filter((request) => request.state === 'open').length,
          },
        ];
      } catch {
        return [swarm.id, null];
      }
    })
  );
  return Object.fromEntries(entries);
}

/** Число роёв каждого проекта для каталога проектов (#255): список
 * проектов его не несёт, поэтому по запросу на проект, не больше предела;
 * отказ одного проекта -- null, «неизвестно». */
export async function loadSwarmCounts(
  projects: Array<{ id: string }>,
  list: (projectId: string) => Promise<unknown[]>,
  limit = ACTIVITY_LIMIT
): Promise<Record<string, number | null>> {
  const entries = await Promise.all(
    projects.slice(0, limit).map(async (project): Promise<[string, number | null]> => {
      try {
        return [project.id, (await list(project.id)).length];
      } catch {
        return [project.id, null];
      }
    })
  );
  return Object.fromEntries(entries);
}

/** Тоны #254 по смыслу: внимание оператора, работа, проблема, ничего. */
export type ActivityTone = 'attention' | 'progress' | 'problem' | 'neutral';

/** Ждущий рой требует оператора, вставший -- тоже, но это уже проблема;
 * идущий работает; остальное нейтрально. */
export function executionActivityTone(execution: string): ActivityTone {
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

/** Локальный маппинг тонов #254 на нынешние тона бейджей, пока
 * access/state.ts не завёл attention/progress/problem/neutral сам. */
export function badgeTone(tone: ActivityTone): BadgeTone {
  switch (tone) {
    case 'attention':
      return 'warning';
    case 'progress':
      return 'success';
    case 'problem':
      return 'danger';
    default:
      return 'muted';
  }
}

const RANK: Record<string, number> = {
  waiting: 0,
  failed: 1,
  running: 2,
  idle: 3,
  succeeded: 4,
  stopped: 5,
};

function rankOf(activity: SwarmActivity | null | undefined): number {
  if (!activity) return 6;
  return RANK[activity.execution] ?? 6;
}

/** Порядок списка: ждущие ответа сверху (у кого вопросов больше -- выше),
 * затем вставшие, идущие, не запущенные, завершённые, остановленные;
 * без сводки (неактивные, не опрошенные, отказ) -- в конце. При равенстве
 * порядок сервера сохраняется: компаратор для стабильной сортировки. */
export function swarmOrder(
  activities: Record<string, SwarmActivity | null | undefined>
): (a: AccessSwarm, b: AccessSwarm) => number {
  const activityOf = (swarm: AccessSwarm) => swarmActivityOf(swarm) ?? activities[swarm.id];
  return (a, b) => {
    const first = activityOf(a);
    const second = activityOf(b);
    const byRank = rankOf(first) - rankOf(second);
    if (byRank !== 0) return byRank;
    return (second?.openRequests ?? 0) - (first?.openRequests ?? 0);
  };
}

/** Число ждущих роёв -- для счётчика у пункта меню. */
export function waitingCount(
  swarms: AccessSwarm[] | null,
  activities: Record<string, SwarmActivity | null | undefined>
): number {
  return (swarms ?? []).filter(
    (swarm) => (swarmActivityOf(swarm) ?? activities[swarm.id])?.execution === 'waiting'
  ).length;
}
