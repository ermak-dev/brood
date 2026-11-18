// BUG: Отказ до регистрации делает реальный trace недекодируемым.
// HYPOTHESIS: Счётчик логически завершённых job отсутствует в формуле open.
// SOLUTION: Явный unregisteredFailed, сумма проверяется без фиктивных детей.
// RESULT: FIXED — реальный Go API и desktop/phone route читаются без эффектов.
// STATUS: FIXED
import { expect, test } from 'bun:test';
import { parseChildGroup, parseChildGroupPage } from '@/services/brood/children';
import { childGroup, childTask } from './fixtures/swarmChildren';

test('unregistered failures settle slots without fabricating children', () => {
  const raw = {
    ...childGroup(),
    registered: 0,
    started: 0,
    unregisteredFailed: 1,
    open: 1,
    children: [],
  };
  const group = parseChildGroup(raw);
  expect(group).toEqual(raw);
  expect(parseChildGroupPage({ items: [raw], nextBefore: 0 }).items[0]).toMatchObject({
    registered: 0,
    unregisteredFailed: 1,
    open: 1,
  });
  expect(parseChildGroup({ ...raw, unregisteredFailed: 2, open: 0 }).children).toEqual([]);
  expect(
    parseChildGroup({
      ...raw,
      registered: 1,
      started: 1,
      open: 0,
      children: [childTask({ state: 'completed', outcome: 'success' })],
    }).children
  ).toHaveLength(1);
});

test('unregistered failure count cannot mask malformed or double-counted slots', () => {
  for (const count of [null, -1, 1.5, '1', 3]) {
    expect(() => parseChildGroup({ ...childGroup(), unregisteredFailed: count })).toThrow(
      'Invalid child history response'
    );
  }
  expect(() => parseChildGroup({ ...childGroup(), unregisteredFailed: 2 })).toThrow(
    'Invalid child history response'
  );
  expect(() => parseChildGroup({ ...childGroup(), unregisteredFailed: 1, open: 2 })).toThrow(
    'Invalid child history response'
  );
});
