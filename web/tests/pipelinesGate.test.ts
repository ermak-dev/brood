// FEATURE: Human gate panel of a pipeline run (#169).
// SPEC: Question and accelerator hints visible; the answer needs a ready
// state, a confirmation kind asks twice, and a refusal stays on the panel.
import { describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import {
  gateDecision,
  gateVisitKey,
  initialGateState,
  selectOption,
} from '@/features/pipelines/state';
import { HumanGatePanel } from '@/features/pipelines/HumanGatePanel';
import type { HumanGate } from '@/services/brood/pipelines';
import en from '@/i18n/locales/en.json';

async function markup(gate: HumanGate) {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  return renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(HumanGatePanel, {
      gate,
      submit: async () => {},
    }))
  );
}

const choiceGate: HumanGate = {
  runId: 'run-1',
  nodeId: 'gate-1',
  visitSequence: 1,
  question: 'Land the release branch?',
  kind: 'multiple_choice',
  options: [
    { id: 'yes', label: 'Land it', accelerator: 'Y' },
    { id: 'no', label: 'Hold', accelerator: 'N' },
  ],
};

const freeformGate: HumanGate = { ...choiceGate, kind: 'freeform', options: [] };

describe('gate decision state machine', () => {
  test('each run, node and visit has a distinct panel identity', () => {
    const key = gateVisitKey(choiceGate);
    expect(gateVisitKey({ ...choiceGate })).toBe(key);
    expect(gateVisitKey({ ...choiceGate, visitSequence: 2 })).not.toBe(key);
    expect(gateVisitKey({ ...choiceGate, nodeId: 'other' })).not.toBe(key);
    expect(gateVisitKey({ ...choiceGate, runId: 'other' })).not.toBe(key);
  });

  test('an unanswered gate sends nothing; a choice sends its option id', () => {
    expect(gateDecision(choiceGate, initialGateState())).toEqual({ act: 'none' });
    const chosen = selectOption(initialGateState(), 'yes');
    expect(gateDecision(choiceGate, chosen)).toEqual({ act: 'send', answer: 'yes' });
  });

  test('a removed option cannot be submitted after a gate refresh', () => {
    expect(gateDecision(choiceGate, selectOption(initialGateState(), 'removed'))).toEqual({ act: 'none' });
  });

  test('freeform sends trimmed text and never an empty answer', () => {
    expect(gateDecision(freeformGate, { ...initialGateState(), freeform: '   ' })).toEqual({
      act: 'none',
    });
    expect(
      gateDecision(freeformGate, { ...initialGateState(), freeform: ' ship it ' })
    ).toEqual({ act: 'send', answer: 'ship it' });
  });

  test('a confirmation kind asks again before it sends', () => {
    const confirmation: HumanGate = {
      ...choiceGate,
      kind: 'confirmation',
      options: [{ id: 'ok', label: 'Destroy the cluster' }],
    };
    const chosen = selectOption(initialGateState(), 'ok');
    expect(gateDecision(confirmation, chosen)).toEqual({ act: 'confirm' });
    expect(gateDecision(confirmation, { ...chosen, confirming: true })).toEqual({
      act: 'send',
      answer: 'ok',
    });
  });

  test('picking another option after confirming starts the question over', () => {
    const first = selectOption({ ...initialGateState(), confirming: true }, 'yes');
    expect(selectOption(first, 'no').confirming).toBe(false);
  });
});

describe('gate panel markup', () => {
  test('shows the question, options with accelerator hints, and no textarea', async () => {
    const html = await markup(choiceGate);
    expect(html).toContain('Land the release branch?');
    expect(html).toContain('Land it');
    expect(html).toContain('Y');
    expect(html).toContain('Hold');
    expect(html).not.toContain('<textarea');
  });

  test('freeform renders a textarea and no option buttons', async () => {
    const html = await markup(freeformGate);
    expect(html).toContain('<textarea');
    expect(html).not.toContain('Land it');
  });

  test('a gate without options says so instead of an empty button row', async () => {
    const empty: HumanGate = { ...choiceGate, options: [] };
    const html = await markup(empty);
    expect(html).toContain('The gate offers no options');
  });
});
