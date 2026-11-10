/**
 * Опрос страницы роя (#257): один таймер на страницу. Страница владеет
 * таймером и на каждый тик перечитывает исполнение идущего роя; инбокс и
 * роли подписываются на тот же тик, а не заводят свои. Граф перечитывается
 * только когда исполнение изменилось в том, что рисуется: ревизия снимка,
 * текущий узел, статус. Роли — только пока исполнение не идёт и не ждёт:
 * в это время сервер привязок не меняет. В скрытой вкладке таймер молчит.
 * Явные перечитывания после ответа, запуска и остановки остаются у своих
 * действий: они не ждут тика.
 */
import { useEffect, useRef } from 'react';
import { createRefreshRegistry, type HeaderRefreshHandler } from '@/hooks/useHeaderRefresh';
import { useVisibleInterval } from '@/hooks/useInterval';
import type { PipelineRun } from '@/services/brood/pipelines';

/** Период опроса страницы роя. */
export const SWARM_POLL_MS = 5000;

const ticks = createRefreshRegistry();

/** Один тик страницы: все подписчики перечитывают своё. Тесты зовут его
 * вместо ожидания таймера. */
export const tickSwarmPolling = () => ticks.refresh();

/** Владелец таймера: страница роя. Тикает, пока live и вкладка видна; на
 * тик зовёт свой обработчик и подписчиков useSwarmTick. */
export function useSwarmPolling(handler: HeaderRefreshHandler, live: boolean) {
  useSwarmTick(handler, live);
  useVisibleInterval(tickSwarmPolling, live ? SWARM_POLL_MS : null);
}

/** Подписка на тик страницы, пока enabled. Обработчик — стабильная ссылка:
 * его смена переподписывает. */
export function useSwarmTick(handler: HeaderRefreshHandler, enabled = true) {
  useEffect(() => {
    if (enabled) return ticks.subscribe(handler);
  }, [enabled, handler]);
}

/** Что из исполнения рисуется на графе: ревизия снимка, текущий узел и
 * статус. Остальное (отчёты, моменты, акторы шагов) SVG не меняет. */
const graphKey = (run: PipelineRun) =>
  [run.definition?.revision ?? '', run.currentNode ?? '', run.status].join('\n');

/** Перечитать ли граф после нового чтения исполнения. У роя без графа
 * нечего перечитывать; первое чтение сравнивать не с чем, граф грузит
 * страница сама. */
export function shouldRefetchGraph(prev: PipelineRun | null, next: PipelineRun | null): boolean {
  if (!next) return false;
  if (!prev) return true;
  return graphKey(prev) !== graphKey(next);
}

const frozenExecutions = new Set(['running', 'waiting']);

/** Перечитывать ли роли по тику: пока исполнение идёт или ждёт ответа,
 * сервер привязок не меняет, а их смена по концу исполнения приходит
 * ключом перечитывания ролей (rolesRefreshKey). */
export function shouldRefetchRoles(execution: string): boolean {
  return !frozenExecutions.has(execution);
}

/** Граф по исполнению: перечитывается, когда новое чтение исполнения
 * изменило то, что рисуется. run undefined — исполнение ещё не прочитано;
 * null — графа нет, и его первое появление грузит сама страница. */
export function useGraphRefetch(
  run: PipelineRun | null | undefined,
  refresh: () => Promise<void>
) {
  const seen = useRef<PipelineRun | null | undefined>(undefined);
  useEffect(() => {
    if (run === undefined) return;
    const prev = seen.current;
    seen.current = run;
    if (prev === undefined || prev === null) return;
    if (shouldRefetchGraph(prev, run)) void refresh();
  }, [run, refresh]);
}
