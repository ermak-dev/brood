// FEATURE: The project workflow catalog the first start of a swarm picks
// from (#196, #218): published definitions and the graph of one version.
// SPEC: Identifiers are escaped, a publication without a revision fails
// closed, the graph is read only by its version and revision, and refusals
// keep the server reason.
import { afterAll, afterEach, expect, test } from 'bun:test';
import { pipelinesApi } from '../src/services/brood/pipelines';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { RunGraph } from '../src/features/pipelines/RunGraph';
import { installTestDOM } from './helpers/dom';
import en from '../src/i18n/locales/en.json';

// DOM-глобалы на время файла: RunGraph парит серверный SVG.
const restoreDOM = installTestDOM();
afterAll(restoreDOM);

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
