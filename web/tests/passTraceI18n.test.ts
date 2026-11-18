import { expect, test } from 'bun:test';
import en from '@/i18n/locales/en.json';
import ru from '@/i18n/locales/ru.json';
import zhCN from '@/i18n/locales/zh-CN.json';
import zhTW from '@/i18n/locales/zh-TW.json';
import vi from '@/i18n/locales/vi.json';
import ko from '@/i18n/locales/ko.json';

const keys = [
  'title',
  'open',
  'back',
  'download',
  'read_only',
  'historical',
  'visits',
  'requests',
  'unscoped',
  'unscoped_hint',
  'gaps',
  'instruction',
  'child_instruction',
  'child_transport_hint',
  'attempts',
  'reported',
  'actual',
  'not_applied',
  'artifact',
  'policy',
  'details',
  'request_context',
  'missing_receipt',
  'unknown_time',
  'graph',
  'observation',
  'source',
  'definition',
];
for (const [language, document] of Object.entries({ en, ru, zhCN, zhTW, vi, ko })) {
  test(`pass trace has localized read-only labels in ${language}`, () => {
    const trace = (document as unknown as Record<string, Record<string, unknown>>).pass_trace;
    expect(trace).toBeDefined();
    for (const key of keys) expect(typeof trace[key]).toBe('string');
    const ephemeral = trace.ephemeral as Record<string, unknown>;
    expect(ephemeral).toBeDefined();
    expect(Object.keys(ephemeral).sort()).toEqual(Object.keys(en.pass_trace.ephemeral).sort());
    for (const value of Object.values(ephemeral)) {
      expect(typeof value).toBe('string');
      expect(String(value).trim().length).toBeGreaterThan(0);
    }
    expect(trace.title).toContain('{{pass}}');
    expect(trace.child_instruction).toContain('{{id}}');
    expect(trace.request_context).toContain('{{pass}}');
    expect(trace.request_context).toContain('{{sequence}}');
  });
}
