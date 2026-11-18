// FEATURE: Сохранённые инструкции детей в существующем trace UI.
// SPEC: Исходный frame и попытки связаны с child ID и UID/inc, не с живым актором;
// квитанция не является отчётом/исходом ребёнка или разрешением cleanup/landing.
// Отсутствующее поле старого API даёт [], брак отклоняется, неизвестность сохраняется.
// IMPLEMENTATION: Общий decoder доставки и disclosure сохраняют frame/попытки;
// отдельная подпись называет child ID, исходные UID/inc и границу свидетельства.
// RESULT: IMPLEMENTED — frame/попытки показаны отдельно от отчёта, legacy без подмены.
// STATUS: DONE
import { afterAll, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { PassTraceContent } from '@/features/swarms/SwarmPassTrace';
import { parsePassTrace } from '@/services/brood/passTrace';
import en from '@/i18n/locales/en.json';
import {
  childFrame as frame,
  childInstruction,
  childTrace,
  traceDelivery,
} from './fixtures/passTrace';
import { childGroup } from './fixtures/swarmChildren';
import { installTestDOM } from './helpers/dom';

const restore = installTestDOM();
afterAll(restore);
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

async function withTrace(value: unknown, check: (container: HTMLElement) => Promise<void>) {
  const trace = parsePassTrace(value);
  const container = document.createElement('div');
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        createElement(I18nextProvider, { i18n }, createElement(PassTraceContent, { trace }))
      )
    );
    await check(container);
  } finally {
    await act(async () => root.unmount());
    container.remove();
  }
}
async function open(details: HTMLDetailsElement) {
  await act(async () => {
    details.open = true;
    details.dispatchEvent(new window.Event('toggle'));
  });
}

test('retained child frame and attempts render independently of completed report and main instruction', async () => {
  await withTrace(childTrace(), async (container) => {
    const child = [...container.querySelectorAll('details')].find((node) =>
      node.querySelector('summary')?.textContent?.includes('Child instruction and delivery')
    );
    expect(child).toBeDefined();
    expect(child!.querySelector('summary')!.textContent).toContain('child-1');
    await open(child!);
    expect(child!.querySelector('pre')!.textContent).toBe(frame);
    expect(child!.textContent).toContain('removed-executor / 4');
    expect(child!.textContent).toContain('Transport evidence only');
    expect(child!.textContent).not.toContain('Independent completed report.');
    const attempts = child!.querySelectorAll('ol > li');
    expect(attempts).toHaveLength(2);
    expect(attempts[0].textContent).toContain('send');
    expect(attempts[0].textContent).toContain('unconfirmed');
    expect(attempts[1].textContent).toContain('probe');
    expect(attempts[1].textContent).toContain('No receipt retained');
    expect(attempts[1].textContent).not.toMatch(/not_sent|success|duplicate|delivered/);
    await open(attempts[1].querySelector('details')!);
    const pending = JSON.parse(attempts[1].querySelector('pre')!.textContent!);
    expect(pending).not.toHaveProperty('outcome');
    expect(pending).not.toHaveProperty('observedAt');
    expect(attempts[1].querySelectorAll('time')).toHaveLength(1);
    expect(attempts[1].querySelector('time')!.getAttribute('dateTime')).toBe(
      '2026-11-17T00:02:00Z'
    );
    const main = [...container.querySelectorAll('details')].filter(
      (node) => node.querySelector('summary')?.textContent === 'Original instruction and delivery'
    );
    expect(main).toHaveLength(1);
    await open(main[0]);
    expect(main[0].querySelector('pre')!.textContent).toBe(traceDelivery().frame);
    const reports = [...container.querySelectorAll('details')].find(
      (node) => node.querySelector('summary')?.textContent === en.swarm_children.title
    )!;
    await open(reports);
    const group = JSON.parse(reports.querySelector('pre')!.textContent!);
    expect(group.children[0]).toMatchObject({
      id: 'child-1',
      state: 'completed',
      outcome: 'success',
      report: 'Independent completed report.',
      delivery: 'unconfirmed',
    });
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('form')).toBeNull();
    expect(container.querySelector('button')).toBeNull();
  });
});

test('duplicate receipt stays transport data and never creates an independent child result', async () => {
  const delivery = childInstruction();
  const duplicate = { ...delivery.attempts[0], mode: 'probe', outcome: 'duplicate' };
  const value = childTrace([childInstruction({ attempts: [duplicate] })]);
  value.steps[0].children = childGroup({ parentSequence: 1 });
  await withTrace(value, async (container) => {
    const child = [...container.querySelectorAll('details')].find((node) =>
      node.querySelector('summary')?.textContent?.includes('Child instruction and delivery')
    );
    expect(child).toBeDefined();
    await open(child!);
    expect(child!.textContent).toContain('duplicate');
    expect(child!.textContent).toContain('Transport evidence only');
    expect(child!.textContent).not.toContain('success');
    expect(parsePassTrace(value).steps[0].children!.children[0].outcome).toBe('');
  });
});

test('unprepared and legacy children do not acquire instructions', async () => {
  for (const instructions of [[], undefined]) {
    const value = childTrace(instructions);
    if (instructions === undefined) delete value.steps[0].childInstructions;
    expect(parsePassTrace(value).steps[0].childInstructions).toEqual([]);
    await withTrace(value, async (container) => {
      expect(container.textContent).not.toContain('Child instruction and delivery');
      expect(container.textContent).toContain('Original instruction and delivery');
    });
  }
});

test('child trace decoder preserves frame bytes and validates attempts instead of swallowing malformed arrays', () => {
  const parsed = parsePassTrace(childTrace());
  expect(parsed.steps[0].childInstructions[0].frame).toBe(frame);
  expect(parsed.steps[0].childInstructions[0].attempts[1]).not.toHaveProperty('outcome');
  for (const invalid of [
    null,
    {},
    'bad',
    [null],
    [childInstruction({ sourceKind: 'step' })],
    [childInstruction({ actorIncarnation: 0 })],
    [
      childInstruction({
        attempts: [{ ...childInstruction().attempts[1], number: 1, outcome: 'not_sent' }],
      }),
    ],
  ]) {
    expect(() => parsePassTrace(childTrace(invalid))).toThrow('Invalid pass trace response');
  }
});
