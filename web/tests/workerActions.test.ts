import { describe, expect, test } from 'bun:test';
import { workerAction } from '@/features/substrate/workerActions';

// BUG: осушение было единственным действием, и осушённый воркер оставался
// тупиком: ёмкость заперта, вернуть её из панели нечем (#89).
// SPEC: действие следует из состояния. Осушённому нужна замена, работающему --
// осушение, а воркеру, который ещё поднимается, не нужно ничего: он станет
// рабочим сам, и замена прервала бы его без причины.

describe('the action offered follows from the state', () => {
  test('a drained worker offers replacement, the only way its capacity returns', () => {
    expect(workerAction('draining')).toBe('replace');
  });

  test('a working worker offers draining', () => {
    expect(workerAction('active')).toBe('drain');
  });

  test('a worker that is still coming up offers nothing', () => {
    expect(workerAction('unavailable')).toBeNull();
  });

  test('an unknown state offers nothing rather than guessing', () => {
    for (const state of ['', 'unspecified', 'retired']) expect(workerAction(state)).toBeNull();
  });
});
