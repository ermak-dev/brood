import { afterAll, afterEach, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { pipelinesApi } from '../src/services/brood/pipelines';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { WorkflowCatalogView } from '../src/features/pipelines/WorkflowCatalog';
import { selectedGraphKey } from '../src/features/pipelines/catalogState';
import { RunGraph } from '../src/features/pipelines/RunDetailPage';
import { installTestDOM } from './helpers/dom';
import en from '../src/i18n/locales/en.json';

// DOM-глобалы на время файла: RunGraph парит серверный SVG.
const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const render = async (props: Parameters<typeof WorkflowCatalogView>[0]) => {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  return renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(WorkflowCatalogView, props))
  );
};

test('definition graph uses a distinct accessible label and renders inline', async () => {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  const html = renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(RunGraph, {
        svg: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10" onload="alert(1)"></svg>',
        label: 'Definition graph',
      })
    )
  );
  // #214: граф в документе с доступным именем, обработчики выброшены.
  expect(html).toContain('aria-label="Definition graph"');
  expect(html).toContain('<svg');
  expect(html).not.toContain(' onload=');
  expect(html).not.toContain('data:image/svg+xml');
});

test('catalog distinguishes loading, error, empty and published versions', async () => {
  const base = {
    definitions: null,
    loading: false,
    error: '',
    retry: async () => {},
    select: () => {},
  };
  expect(await render({ ...base, loading: true })).toContain('role="status"');
  expect(await render({ ...base, error: 'catalog unavailable' })).toContain('catalog unavailable');
  expect(await render({ ...base, definitions: [] })).toContain('No published workflows');
  const html = await render({
    ...base,
    definitions: [
      {
        name: 'release',
        version: 3,
        revision: '0123456789abcdef0123456789abcdef',
        publishedAt: null,
      },
    ],
  });
  expect(html).toContain('release');
  expect(html).toContain('3');
});

test('graph identity follows project, name and published version and hides during catalog failures or refresh', () => {
  const definitions = [
    {
      name: 'release',
      version: 1,
      revision: '0123456789abcdef0123456789abcdef',
      publishedAt: null,
    },
  ];
  const key = selectedGraphKey('p', 'release', definitions, false, '');
  expect(key).toBeTruthy();
  expect(selectedGraphKey('p', 'release', [{ ...definitions[0], version: 2 }], false, '')).not.toBe(
    key
  );
  expect(
    selectedGraphKey(
      'p',
      'release',
      [{ ...definitions[0], revision: 'fedcba9876543210fedcba9876543210' }],
      false,
      ''
    )
  ).not.toBe(key);
  expect(selectedGraphKey('other', 'release', definitions, false, '')).not.toBe(key);
  expect(selectedGraphKey('p', 'release', definitions, true, '')).toBeNull();
  expect(selectedGraphKey('p', 'release', definitions, false, 'unavailable')).toBeNull();
  expect(selectedGraphKey('p', 'missing', definitions, false, '')).toBeNull();
});

const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
});

test('reads published definitions and their graphs with escaped identifiers', async () => {
  const calls: string[] = [];
  globalThis.fetch = (async (url: string) => {
    calls.push(url);
    return new Response(
      JSON.stringify(
        calls.length === 1
          ? {
              workflows: [
                {
                  name: 'release/a',
                  version: 3,
                  revision: '0123456789abcdef0123456789abcdef',
                  publishedAt: null,
                },
              ],
            }
          : { svg: '<svg></svg>' }
      ),
      { status: 200 }
    );
  }) as typeof fetch;
  expect(await pipelinesApi.listDefinitions('project/a')).toEqual([
    {
      name: 'release/a',
      version: 3,
      revision: '0123456789abcdef0123456789abcdef',
      publishedAt: null,
    },
  ]);
  expect(
    await pipelinesApi.getDefinitionGraph(
      'project/a',
      'release/a',
      3,
      '0123456789abcdef0123456789abcdef'
    )
  ).toEqual({ svg: '<svg></svg>' });
  expect(calls).toEqual([
    '/api/projects/project%2Fa/workflow-catalog',
    '/api/projects/project%2Fa/workflows/release%2Fa/graph?version=3&revision=0123456789abcdef0123456789abcdef',
  ]);
});

test('a catalog without a valid publication revision fails closed', async () => {
  globalThis.fetch = (async () =>
    new Response(
      JSON.stringify({ workflows: [{ name: 'release', version: 1, publishedAt: null }] }),
      { status: 200 }
    )) as typeof fetch;
  await expect(pipelinesApi.listDefinitions('p')).rejects.toThrow();
});

test('retrying a graph failure refreshes the catalog before fetching the graph', () => {
  const source = readFileSync(
    new URL('../src/features/pipelines/WorkflowCatalog.tsx', import.meta.url),
    'utf8'
  );
  expect(source).toContain('await catalog.refresh()');
  expect(source).toContain('setGraphAttempt((attempt) => attempt + 1)');
  expect(source).toContain('retry={refreshCatalog}');
  expect(source).toContain('refreshCatalog={refreshCatalog}');
  expect(source).toContain('key={`${graphKey}:${graphAttempt}`}');
  expect(source).not.toContain('retry={graph.refresh}');
});

test('a changed publication never falls back to an unversioned graph request', async () => {
  const calls: string[] = [];
  globalThis.fetch = (async (url: string) => {
    calls.push(url);
    return new Response(JSON.stringify({ detail: 'workflow version changed' }), { status: 409 });
  }) as typeof fetch;
  await expect(
    pipelinesApi.getDefinitionGraph('p', 'release', 1, '0123456789abcdef0123456789abcdef')
  ).rejects.toMatchObject({ status: 409, message: 'workflow version changed' });
  expect(calls).toEqual([
    '/api/projects/p/workflows/release/graph?version=1&revision=0123456789abcdef0123456789abcdef',
  ]);
});

test('empty catalog remains empty and graph errors retain their reason', async () => {
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ workflows: [] }), { status: 200 })) as typeof fetch;
  expect(await pipelinesApi.listDefinitions('p')).toEqual([]);
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ detail: 'workflow not found' }), {
      status: 404,
    })) as typeof fetch;
  await expect(
    pipelinesApi.getDefinitionGraph('p', 'missing', 1, '0123456789abcdef0123456789abcdef')
  ).rejects.toMatchObject({ status: 404, message: 'workflow not found' });
});
