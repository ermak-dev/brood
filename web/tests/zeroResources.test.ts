import { expect, test } from 'bun:test';
import { DASH, formatCpu, formatMemory } from '@/features/substrate/format';

// BUG: Явный ноль отображался как отсутствие данных (#93).
// SPEC: Нулевая загрузка видна числом с единицей, неизвестная остаётся прочерком.
test('zero worker usage is not missing data', () => {
  for (const zero of [0, '0']) {
    expect(formatCpu(zero)).toBe('0 CPU');
    expect(formatMemory(zero)).toBe('0 MiB');
  }
});

test('unknown and invalid resource values are still unknown', () => {
  for (const missing of [undefined, '', 'not-a-number']) {
    expect(formatCpu(missing)).toBe(DASH);
    expect(formatMemory(missing)).toBe(DASH);
  }
});
