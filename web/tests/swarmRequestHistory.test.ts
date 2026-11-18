import { expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter } from 'react-router-dom';
import { RequestInbox } from '@/features/swarms/SwarmRequests';
import type { RequestInboxModel } from '@/features/swarms/useSwarmRequests';
import type { SwarmRequest } from '@/services/brood/requests';
import en from '@/i18n/locales/en.json';

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

for (const scenario of [
  { states: ['cancelled'], summary: '1 cancelled' },
  { states: ['answered'], summary: '1 answered' },
  { states: ['answered', 'cancelled', 'cancelled'], summary: '1 answered · 2 cancelled' },
] as const) {
  test(`request history counts actual states: ${scenario.states.join(', ')}`, () => {
    const requests: SwarmRequest[] = scenario.states.map((state, index) => ({
      id: `request-${index}`,
      swarmId: 'candidate',
      origin: 'node',
      sequence: index + 1,
      nodeId: 'ask',
      kind: 'choice',
      question: 'Approve?',
      context: '',
      options: [{ label: 'Different choice', outcome: 'success' }],
      state,
      createdAt: '2026-11-17T20:52:00Z',
    }));
    const model: RequestInboxModel = {
      requests,
      error: '',
      refresh: async () => {},
      canAnswer: false,
      notices: {},
      handledHere: [],
      retriedHere: [],
      focus: null,
      focused: () => {},
      answer: async () => {},
      reach: () => {},
      deliver: async () => {},
    };
    const html = renderToStaticMarkup(
      createElement(
        I18nextProvider,
        { i18n },
        createElement(MemoryRouter, null, createElement(RequestInbox, { model }))
      )
    );
    const summaries = [...html.matchAll(/<summary\b[^>]*>(.*?)<\/summary>/g)].map((match) =>
      match[1].replace(/<[^>]*>/g, '').trim()
    );
    expect(summaries).toContain(scenario.summary);
    for (const state of ['answered', 'cancelled'] as const) {
      expect((html.match(new RegExp(`data-state="${state}"`, 'g')) ?? []).length).toBe(
        requests.filter((request) => request.state === state).length
      );
    }
  });
}
