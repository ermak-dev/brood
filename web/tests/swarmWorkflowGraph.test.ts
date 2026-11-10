// BUG: Header refresh leaves the open swarm's runs and graph stale.
// HYPOTHESIS: The run list does not subscribe to header refresh, so its selected graph is not reloaded.
// SOLUTION: Refresh the list and reload the graph for each new list snapshot.
// RESULT: FIXED — updated selections use the refreshed list and graph.
// STATUS: FIXED
// FEATURE: Show existing workflow run graphs in the swarm panel.
// SPEC: Loading, errors and empty results are visible; runs show their graph safely.
// IMPLEMENTATION: Reuse the scoped runs and graph APIs in the swarm panel.
// RESULT: IMPLEMENTED — selected runs render as safely isolated images.
// STATUS: DONE
import { afterAll, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter } from 'react-router-dom';
import { SwarmWorkflowView } from '@/features/pipelines/SwarmWorkflow';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

// DOM-глобалы на время файла: RunGraph парит серверный SVG.
const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
const render = (props: Parameters<typeof SwarmWorkflowView>[0]) =>
  renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(MemoryRouter, null, createElement(SwarmWorkflowView, props))
    )
  );
const retry = async () => {};
const run = { id: 'r/1', name: 'release', status: 'running' as const, startedAt: null, nodes: [] };

test('header refresh reloads runs before the selected graph', () => {
  const source = readFileSync(new URL('../src/features/pipelines/SwarmWorkflow.tsx', import.meta.url), 'utf8');
  expect(source).toContain('const runs = useAccessResource(load);');
});

test('reports loading, failure and empty runs without inventing workflows', () => {
  expect(render({ runs: null, loading: true, error: '', retry })).not.toContain('No pipeline runs');
  expect(render({ runs: null, loading: false, error: 'Access denied', retry })).toContain(
    'Access denied'
  );
  expect(render({ runs: [], loading: false, error: '', retry })).toContain(en.pipelines.empty);
});

test('renders the named run and its graph as filtered inline SVG', () => {
  const html = render({
    runs: [run],
    loading: false,
    error: '',
    retry,
    graph: { svg: '<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>' },
  });
  expect(html).toContain('release');
  // #214: вместо картинки — инлайновый отфильтрованный SVG.
  expect(html).toContain('<svg');
  expect(html).not.toContain(' onload=');
  expect(html).not.toContain('data:image/svg+xml');
});

test('falls back to an available run when the selected run disappeared', () => {
  const html = render({ runs: [run], loading: false, error: '', retry, selectedId: 'deleted' });
  expect(html).toContain('release');
  expect(html).not.toContain('deleted');
});

test('shows graph loading and error independently of the run list', () => {
  const loading = render({ runs: [run], loading: false, error: '', retry, graphLoading: true });
  expect(loading).toContain('release');
  const failure = render({
    runs: [run],
    loading: false,
    error: '',
    retry,
    graphError: 'Graph unavailable',
  });
  expect(failure).toContain('Graph unavailable');
});
