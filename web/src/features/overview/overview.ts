/**
 * Обзор проекта (#197): что ждёт оператора, что идёт и что встало -- по
 * строкам списка роёв, без запроса на рой. Исполнение и открытые вопросы
 * несёт строка `GET /api/projects/{id}/swarms` (#234), недоставленные
 * ответы актору -- она же (#266); сводка читается одной выборкой на весь
 * список. Прогон -- это исполнение роя (#206): прогон роя -- его строка.
 */
import type { AccessSwarm } from '@/services/brood/access';
import { swarmActivityOf, swarmOrder } from '@/features/access/swarmActivity';
import { SWARMS_PATH, swarmPath } from '@/features/swarms/routes';

/** Строка списка роёв с числом недоставленных ответов (#266). Поле читает
 * только обзор; тип строки списка панель расширит вместе с признаком в
 * списке роёв. */
export type OverviewRow = AccessSwarm & { undeliveredAnswers?: number };

/** Сколько роёв обзор показывает, если ни один не ждёт оператора и не идёт. */
export const OVERVIEW_SWARMS = 6;

export interface OverviewSummary {
  total: number;
  /** Строки без сводки исполнения или с браком в ней: о них обзор не знает,
   * в группы ниже они не входят. */
  unknown: number;
  /** Ждут оператора: открытый вопрос узла или актора, или граф ждёт ответа. */
  waiting: OverviewRow[];
  openRequests: number;
  /** Рои с ответами актору, ждущими повтора доставки; null -- сервер их не
   * считает (строки без поля), это не ноль. */
  undelivered: OverviewRow[] | null;
  undeliveredAnswers: number | null;
  running: OverviewRow[];
  /** Исполнение встало (`failed`). */
  stuck: OverviewRow[];
}

/** Число недоставленных ответов строки; брак и отсутствие поля -- undefined. */
export function undeliveredOf(row: OverviewRow): number | undefined {
  const value = row.undeliveredAnswers;
  return Number.isSafeInteger(value) && (value as number) >= 0 ? value : undefined;
}

/** Сводка обзора по строкам списка. Строка с браком сводки (дробное число
 * вопросов отвергает swarmActivityOf, отрицательное -- activityOf)
 * неизвестна и в группы не входит. */
export function overviewSummary(rows: OverviewRow[]): OverviewSummary {
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
    const undelivered = undeliveredOf(row);
    if (undelivered !== undefined) {
      counted = true;
      summary.undeliveredAnswers = (summary.undeliveredAnswers ?? 0) + undelivered;
      if (undelivered > 0) summary.undelivered?.push(row);
    }
    const activity = activityOf(row);
    if (!activity) {
      summary.unknown += 1;
      continue;
    }
    summary.openRequests += activity.openRequests;
    if (activity.execution === 'waiting' || activity.openRequests > 0) summary.waiting.push(row);
    if (activity.execution === 'running') summary.running.push(row);
    if (activity.execution === 'failed') summary.stuck.push(row);
  }
  if (rows.length > 0 && !counted) {
    summary.undelivered = null;
    summary.undeliveredAnswers = null;
  }
  return summary;
}

/** Сводка исполнения строки (swarmActivityOf), если число вопросов в ней --
 * неотрицательное целое; иначе undefined, «неизвестно». */
export function activityOf(row: OverviewRow) {
  const activity = swarmActivityOf(row);
  return activity && activity.openRequests >= 0 ? activity : undefined;
}

/** Куда ведёт плитка сводки: к единственному рою -- на его страницу, там
 * инбокс и граф; к нескольким -- в список роёв, где ждущие сверху. */
export function summaryTarget(rows: OverviewRow[]): string | undefined {
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
function stepOf(row: OverviewRow): 0 | 1 | 2 {
  const activity = activityOf(row);
  const execution = activity?.execution;
  if (
    execution === 'waiting' ||
    execution === 'failed' ||
    (activity?.openRequests ?? 0) > 0 ||
    (undeliveredOf(row) ?? 0) > 0 ||
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
  rows: OverviewRow[],
  limit = OVERVIEW_SWARMS
): { shown: OverviewRow[]; hidden: number } {
  const ordered = rows
    .map((row) => ({ row, step: stepOf(row) }))
    .sort((a, b) => a.step - b.step || swarmOrder(a.row, b.row));
  const shown = ordered
    .filter(({ step }, index) => step < 2 || index < limit)
    .map(({ row }) => row);
  return { shown, hidden: rows.length - shown.length };
}
