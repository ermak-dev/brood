import { expect, test } from 'bun:test';
import en from '@/i18n/locales/en.json';
import ru from '@/i18n/locales/ru.json';
import zhCN from '@/i18n/locales/zh-CN.json';
import zhTW from '@/i18n/locales/zh-TW.json';
import vi from '@/i18n/locales/vi.json';
import ko from '@/i18n/locales/ko.json';

test('comparison controls and honest outcome/time labels exist in all six locales', () => {
  const keys = [
    'title',
    'open',
    'back',
    'comparison_label',
    'reference',
    'candidate',
    'swarm',
    'pass',
    'submit',
    'empty',
    'read_only',
    'alignment',
    'same_goal',
    'same_source',
    'same_definition',
    'visits',
    'children',
    'counts',
    'duration',
    'duration_delta',
    'unknown',
    'same',
    'different',
    'not_comparable',
    'missing',
    'position',
    'invalid',
    'download',
    'details',
    'actual_outcome',
    'actual_report',
    'reported_outcome',
    'reported_report',
  ];
  for (const locale of [en, ru, zhCN, zhTW, vi, ko]) {
    const values = (locale as unknown as Record<string, Record<string, string>>).pass_compare;
    expect(values).toBeDefined();
    for (const key of keys) expect(typeof values[key]).toBe('string');
    expect(values.position).toContain('{{position}}');
    for (const token of ['reference', 'candidate', 'delta'])
      expect(values.counts).toContain(`{{${token}}}`);
  }
});
