/**
 * Обзор проекта (#197): что ждёт оператора, что идёт и что встало -- по
 * строкам списка роёв, без запроса на рой. Исполнение и открытые вопросы
 * несёт строка `GET /api/projects/{id}/swarms` (#234), недоставленные
 * ответы актору -- она же (#266); сводка читается одной выборкой на весь
 * список. Прогон -- это исполнение роя (#206): прогон роя -- его строка.
 */
import type { AccessSwarm } from '@/services/brood/access';
import { swarmActivityOf, swarmOrder, waitsForOperator } from '@/features/access/swarmActivity';
import { SWARMS_PATH, swarmPath } from '@/features/swarms/routes';

/** Сколько роёв обзор показывает, если ни один не ждёт оператора и не идёт. */
export const OVERVIEW_SWARMS = 6;

/** Сколько роёв показывают «Последние прогоны». */
export const RECENT_RUNS = 5;

/** «Последние прогоны» (#268): рои по моменту последнего исполнения
 * (`executionAt`: начало текущего прохода, остановка или окончание), новые
 * сверху, не больше limit; при равенстве -- порядок сервера. Рой без
 * момента или с браком в нём не входит. [] -- прогонов не было: ни один рой
 * не запускался (или роёв нет). null -- о прогонах неизвестно: рои шли или
 * их сводки нет, а момента не несёт ни одна строка (старый сервер, проходы
 * до записи момента); «прогонов не было» было бы неправдой. */
export function recentRuns(rows: AccessSwarm[], limit = RECENT_RUNS): AccessSwarm[] | null {
  const dated = rows.flatMap((row) => {
    const at = swarmActivityOf(row)?.executionAt;
    return at === undefined ? [] : [{ row, at }];
  });
  if (dated.length === 0) {
    const untouched = rows.every((row) => swarmActivityOf(row)?.execution === 'idle');
    return untouched ? [] : null;
  }
  return dated
    .sort((a, b) => b.at - a.at)
    .slice(0, limit)
    .map(({ row }) => row);
}

export interface OverviewSummary {
  total: number;
  /** Строки без сводки исполнения или с браком в ней: о них обзор не знает,
   * в группы ниже они не входят. */
  unknown: number;
  /** Ждут оператора (waitsForOperator, то же правило у счётчика меню):
   * открытый вопрос узла или актора, или граф ждёт ответа. */
  waiting: AccessSwarm[];
  openRequests: number;
  /** Рои с ответами актору, ждущими повтора доставки; null -- сервер их не
   * считает (строки без поля), это не ноль. */
  undelivered: AccessSwarm[] | null;
  undeliveredAnswers: number | null;
  running: AccessSwarm[];
  /** Исполнение встало (`failed`). */
  stuck: AccessSwarm[];
}

/** Сводка обзора по строкам списка. Строка с браком сводки (swarmActivityOf
 * отвергает дробное и отрицательное число вопросов) неизвестна и в группы
 * не входит. */
export function overviewSummary(rows: AccessSwarm[]): OverviewSummary {
  const summary: OverviewSummary = {
    total: rows.length,
    unknown: 0,
    waiting: [],
    openRequests: 0,
    undelivered: [],
    undeliveredAnswers: 0,
    running: [],
    stuck: [],
  };
  let counted = false;
  for (const row of rows) {
    const activity = swarmActivityOf(row);
    if (!activity) {
      summary.unknown += 1;
      continue;
    }
    const undelivered = activity.undeliveredAnswers;
    if (undelivered !== undefined) {
      counted = true;
      summary.undeliveredAnswers = (summary.undeliveredAnswers ?? 0) + undelivered;
      if (undelivered > 0) summary.undelivered?.push(row);
    }
    summary.openRequests += activity.openRequests;
    if (waitsForOperator(activity)) summary.waiting.push(row);
    if (activity.execution === 'running') summary.running.push(row);
    if (activity.execution === 'failed') summary.stuck.push(row);
  }
  if (rows.length > 0 && !counted) {
    summary.undelivered = null;
    summary.undeliveredAnswers = null;
  }
  return summary;
}

/** Куда ведёт плитка сводки: к единственному рою -- на его страницу, там
 * инбокс и граф; к нескольким -- в список роёв, где ждущие сверху. */
export function summaryTarget(rows: AccessSwarm[]): string | undefined {
  if (rows.length === 0) return undefined;
  return rows.length === 1 ? swarmPath(rows[0].id) : SWARMS_PATH;
}

/** Страница проекта с проектом в адресе: ссылкой можно поделиться. */
export function projectDetailsPath(project: string): string {
  return `/projects/${encodeURIComponent(project)}`;
}

const inProgressStates = new Set(['provisioning', 'deleting']);

/** Ступень роя в обзоре: 0 -- нужен оператор (вопрос, граф ждёт ответа,
 * недоставленный ответ, встало исполнение, сбой инфраструктуры), 1 -- идёт
 * работа (исполнение или операция инфраструктуры), 2 -- остальное. */
function stepOf(row: AccessSwarm): 0 | 1 | 2 {
  const activity = swarmActivityOf(row);
  const execution = activity?.execution;
  if (
    waitsForOperator(activity) ||
    execution === 'failed' ||
    (activity?.undeliveredAnswers ?? 0) > 0 ||
    row.state === 'error'
  ) {
    return 0;
  }
  if (execution === 'running' || inProgressStates.has(row.state)) return 1;
  return 2;
}

/** Какие рои показывает обзор и в каком порядке. Ждущие оператора и идущие
 * -- всегда, пределом не режутся; остальные дополняют список до
 * OVERVIEW_SWARMS. Внутри ступени -- порядок списка роёв (swarmOrder), при
 * равенстве -- порядок сервера. hidden -- сколько роёв осталось в списке. */
export function overviewSwarms(
  rows: AccessSwarm[],
  limit = OVERVIEW_SWARMS
): { shown: AccessSwarm[]; hidden: number } {
  const ordered = rows
    .map((row) => ({ row, step: stepOf(row) }))
    .sort((a, b) => a.step - b.step || swarmOrder(a.row, b.row));
  const shown = ordered
    .filter(({ step }, index) => step < 2 || index < limit)
    .map(({ row }) => row);
  return { shown, hidden: rows.length - shown.length };
}
