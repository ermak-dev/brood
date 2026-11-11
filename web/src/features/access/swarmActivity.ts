/**
 * Исполнение и открытые вопросы роёв в списке (#255). Строка списка роёв
 * несёт их сама (`execution`, `openRequests`, #234; `undeliveredAnswers`,
 * #266): сервер читает сводку одной выборкой на весь список, и панель роёв
 * не опрашивает.
 */
import type { AccessSwarm } from '@/services/brood/access';
import type { BadgeTone } from './state';

export interface SwarmActivity {
  /** Исполнение в словах страницы роя: idle, running, waiting, succeeded,
   * failed, stopped; незнакомое значение остаётся как есть. */
  execution: string;
  openRequests: number;
  /** Ответы актору, ждущие повтора доставки (#266); без поля (старый сервер)
   * или с браком -- undefined, «неизвестно», а не ноль. */
  undeliveredAnswers?: number;
}

/** Сводка из строки списка (#234); без неё или с браком -- undefined,
 * «неизвестно», а не ноль вопросов и не «не запущен». Брак в
 * `undeliveredAnswers` сводку не прячет: теряется только это число. */
export function swarmActivityOf(swarm: AccessSwarm): SwarmActivity | undefined {
  if (typeof swarm.execution !== 'string' || !Number.isSafeInteger(swarm.openRequests)) {
    return undefined;
  }
  const activity: SwarmActivity = {
    execution: swarm.execution,
    openRequests: swarm.openRequests as number,
  };
  if (Number.isSafeInteger(swarm.undeliveredAnswers)) {
    activity.undeliveredAnswers = swarm.undeliveredAnswers;
  }
  return activity;
}

/** Тон исполнения в словаре бейджей #254: внимание оператора, работа, проблема, ничего. */
export type ActivityTone = BadgeTone;

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

const RANK: Record<string, number> = {
  waiting: 0,
  failed: 1,
  running: 2,
  idle: 3,
  succeeded: 4,
  stopped: 5,
};

function rankOf(activity: SwarmActivity | undefined): number {
  if (!activity) return 6;
  return RANK[activity.execution] ?? 6;
}

/** Порядок списка: ждущие ответа сверху (у кого вопросов больше -- выше),
 * затем вставшие, идущие, не запущенные, завершённые, остановленные;
 * без сводки -- в конце. При равенстве порядок сервера сохраняется:
 * компаратор для стабильной сортировки. */
export function swarmOrder(a: AccessSwarm, b: AccessSwarm): number {
  const first = swarmActivityOf(a);
  const second = swarmActivityOf(b);
  const byRank = rankOf(first) - rankOf(second);
  if (byRank !== 0) return byRank;
  return (second?.openRequests ?? 0) - (first?.openRequests ?? 0);
}

/** Число ждущих роёв -- для счётчика у пункта меню. */
export function waitingCount(swarms: AccessSwarm[] | null): number {
  return (swarms ?? []).filter((swarm) => swarmActivityOf(swarm)?.execution === 'waiting').length;
}
