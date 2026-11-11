// FEATURE: Legend of the inline run graph (#254, epic #251).
// SPEC: Color and stroke are not the only carriers of a node's kind and
// state: one legend under the SVG names what the drawing uses. It lists
// only what this graph shows: node kinds with a distinct stroke (actor,
// human, exit) when more than one is present, and the step states present
// in the run, plus "pending" for nodes the run has not reached. A graph
// with nothing to explain (one kind, no states) gets no legend.
import { afterAll, describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { parseServerSVG } from '@/features/pipelines/svgFilter';
import { legendEntries } from '@/features/pipelines/legend';
import { RunGraph } from '@/features/pipelines/RunGraph';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });

const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const node = (id: string, classes: string) =>
  `<g class="${classes}" data-node-id="${id}"><rect x="0" y="0" width="10" height="10"/><text>${id}</text></g>`;
const svg = (...nodes: string[]) =>
  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 120">${nodes.join('')}</svg>`;
const root = (source: string) => parseServerSVG(source)[0];

const render = (source: string) =>
  renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(RunGraph, { svg: source }))
  );

describe('legend entries', () => {
  test('a run names the states present and pending for unreached nodes', () => {
    const entries = legendEntries(
      root(
        svg(
          node('start', 'node node-start state-done'),
          node('plan', 'node node-actor state-active'),
          node('ask', 'node node-human'),
          node('end', 'node node-exit')
        )
      )
    );
    expect(entries).toEqual([
      { group: 'kind', id: 'actor' },
      { group: 'kind', id: 'human' },
      { group: 'kind', id: 'exit' },
      { group: 'state', id: 'pending' },
      { group: 'state', id: 'active' },
      { group: 'state', id: 'done' },
    ]);
  });

  test('engine state names fold onto the marks the drawing uses', () => {
    const entries = legendEntries(
      root(
        svg(
          node('p', 'node node-actor state-pending'),
          node('a', 'node node-actor state-completed'),
          node('b', 'node node-actor state-refused'),
          node('c', 'node node-actor state-stopped'),
          node('d', 'node node-actor state-unconfirmed')
        )
      )
    );
    expect(entries).toEqual([
      { group: 'state', id: 'pending' },
      { group: 'state', id: 'done' },
      { group: 'state', id: 'failed' },
      { group: 'state', id: 'skipped' },
      { group: 'state', id: 'unconfirmed' },
    ]);
  });

  test('a definition preview of one kind has nothing to explain', () => {
    expect(legendEntries(root(svg(node('a', 'node node-actor'), node('b', 'node node-actor'))))).toEqual(
      []
    );
    // Кinds without a distinct stroke do not earn an entry on their own.
    expect(
      legendEntries(root(svg(node('a', 'node node-actor'), node('s', 'node node-start'))))
    ).toEqual([]);
  });

  test('a preview with a human node explains the dashed stroke', () => {
    expect(legendEntries(root(svg(node('a', 'node node-actor'), node('q', 'node node-human'))))).toEqual(
      [
        { group: 'kind', id: 'actor' },
        { group: 'kind', id: 'human' },
      ]
    );
  });

  test('an unknown state (server newer than the panel) is not a legend entry', () => {
    expect(legendEntries(root(svg(node('a', 'node node-actor state-paused'))))).toEqual([]);
  });

  test('no graph, no entries', () => {
    expect(legendEntries(undefined)).toEqual([]);
  });
});

describe('legend markup', () => {
  test('one list under the svg with a swatch and a word per entry', () => {
    const html = render(
      svg(node('plan', 'node node-actor state-active'), node('ask', 'node node-human state-done'))
    );
    // Классы CSS-модуля в bun:test не резолвятся: проверяется структура.
    const lists = html.match(/<ul aria-label="Legend"/g) ?? [];
    expect(lists.length).toBe(1);
    expect(html.indexOf('</svg>')).toBeLessThan(html.indexOf('<ul '));
    expect(html).toContain('>actor<');
    expect(html).toContain('>human<');
    expect(html).toContain('>In progress<');
    expect(html).toContain('>Done<');
    expect(html).not.toContain('pipelines.legend');
    // У каждой записи образец скрыт от скринридера, слово рядом читается.
    expect((html.match(/<li/g) ?? []).length).toBe(4);
    expect((html.match(/aria-hidden="true"><\/span>/g) ?? []).length).toBe(4);
  });

  test('a graph with nothing to explain renders no legend', () => {
    const html = render(svg(node('a', 'node node-actor'), node('b', 'node node-actor')));
    expect(html).not.toContain('<ul');
  });
});
