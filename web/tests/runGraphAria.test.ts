// FEATURE: Accessible names of the run graph (#256, epic #251).
// SPEC: A node button is named by its step, its kind and its state
// ("review, actor, Done"), so a screen reader hears where the run stands
// without the colours; an unknown state falls back to its raw class. The
// svg carries aria-describedby to a text that explains what the nodes are.
// The state words are those of the step history (swarm_page.node_states).
import { afterAll, describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { RunGraph } from '@/features/pipelines/RunGraph';
import { svgNodeState, parseServerSVG } from '@/features/pipelines/svgFilter';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

const node = (id: string, kind: string, state: string) =>
  `<g class="node node-${kind} ${state}" data-node-id="${id}"><rect width="10" height="10"/><text>${id}</text></g>`;
const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">${node(
  'review',
  'actor',
  'state-done'
)}${node('decision', 'human', 'state-active')}${node('verify', 'actor', '')}${node(
  'odd',
  'actor',
  'state-quantum'
)}</svg>`;

const render = (props: Record<string, unknown>) =>
  renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(RunGraph, { svg, ...props }))
  );
const labels = (html: string) => [...html.matchAll(/aria-label="([^"]*)"/g)].map((m) => m[1]);

describe('node state from the engine class', () => {
  test('svgNodeState reads state-<state>, empty when the node has none', () => {
    const [root] = parseServerSVG(svg);
    const children = (root as { props: { children: unknown[] } }).props.children;
    expect(children.map((child) => svgNodeState(child as never))).toEqual([
      'done',
      'active',
      '',
      'quantum',
    ]);
  });
});

describe('run graph aria', () => {
  test('a node button is named by step, kind and state', () => {
    const html = render({ onSelect: () => {} });
    expect(labels(html)).toContain('review, actor, Done');
    expect(labels(html)).toContain('decision, human, Active');
  });

  test('a node without a state is named by step and kind only', () => {
    expect(labels(render({ onSelect: () => {} }))).toContain('verify, actor');
  });

  test('an unknown state falls back to its raw name, never to a missing key', () => {
    const html = render({ onSelect: () => {} });
    expect(labels(html)).toContain('odd, actor, quantum');
    expect(html).not.toContain('node_states');
  });

  test('the svg is described by a text that explains the nodes', () => {
    const html = render({ onSelect: () => {} });
    const described = html.match(/<svg[^>]*aria-describedby="([^"]+)"/);
    expect(described).not.toBeNull();
    const id = described?.[1] ?? '';
    const description = html.match(new RegExp(`<[a-z]+[^>]*id="${id}"[^>]*>([^<]*)<`));
    expect(description?.[1]).toBe(en.pipelines.graph_description);
  });
});
