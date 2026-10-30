/** Что предложить для воркера в этом состоянии.
 *
 * `draining` окончательно: планировщик его уже не использует, а под живёт
 * дальше и держит место пула, поэтому ёмкость возвращает только замена.
 * `unavailable` -- переходное: воркер поднимается и станет рабочим сам,
 * замена прервала бы его без причины. Неизвестное состояние не угадываем. */
export type WorkerAction = 'drain' | 'replace' | null;

export function workerAction(state: string): WorkerAction {
  if (state === 'draining') return 'replace';
  if (state === 'active') return 'drain';
  return null;
}
