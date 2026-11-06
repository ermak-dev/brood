// FEATURE: Pipelines section of the panel (#169): run table, run detail, nav.
// SPEC: Live runs sort first; the run row opens the run page; the SVG graph
// lands in the page with the engine's state classes; ordinary users see the
// section; every locale carries the new keys.
import { describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import { createInstance } from 'i18next';
import { readFileSync } from 'node:fs';
import { RunTable } from '@/features/pipelines/PipelinesPage';
import { NodeTimeline, RunGraph } from '@/features/pipelines/RunDetailPage';
import { openGate, runIsLive, runTone, sortRuns } from '@/features/pipelines/state';
import { visibleAccessPath } from '@/features/access/permissions';
import type { PipelineRun } from '@/services/brood/pipelines';
import en from '@/i18n/locales/en.json';

const source = (path: string) =>
  readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');

const run = (over: Partial<PipelineRun>): PipelineRun => ({
  id: 'run-1',
  name: 'release',
  status: 'running',
  startedAt: '2026-11-05T09:00:00Z',
  nodes: [],
  ...over,
});

// BUG: An unconfirmed actor visit had no visible uncertainty in the timeline.
// HYPOTHESIS: The timeline displayed outcomes but not the unconfirmed state.
// SOLUTION: Show the existing localized unknown-outcome label.
// RESULT: FIXED — uncertainty is visible without calling it failure.
// STATUS: FIXED
test('timeline identifies an unconfirmed actor outcome', async () => {
  const i18n = createInstance();
  await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
  const html = renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(NodeTimeline, {
      nodes: [{ id: 'worker', label: 'worker', kind: 'actor', status: 'unconfirmed' }],
    }))
  );
  expect(html).toContain('outcome unknown');
});

describe('run table logic', () => {
  test('live and waiting runs sort ahead of finished ones, newest first', () => {
    const ordered = sortRuns([
      run({ id: 'done-old', status: 'succeeded', startedAt: '2026-11-01T09:00:00Z' }),
      run({ id: 'waiting', status: 'waiting_human', startedAt: '2026-11-04T09:00:00Z' }),
      run({ id: 'failed', status: 'failed', startedAt: '2026-11-03T09:00:00Z' }),
      run({ id: 'running', status: 'running', startedAt: '2026-11-05T09:00:00Z' }),
      run({ id: 'done-new', status: 'succeeded', startedAt: '2026-11-02T09:00:00Z' }),
    ]);
    expect(ordered.map((item) => item.id)).toEqual([
      'running',
      'waiting',
      'failed',
      'done-new',
      'done-old',
    ]);
  });

  test('status tones keep failure red and waiting amber', () => {
    expect(runTone('failed')).toBe('danger');
    expect(runTone('waiting_human')).toBe('warning');
    expect(runTone('running')).toBe('success');
    expect(runTone('succeeded')).toBe('success');
  });

  test('the table lists rows with name, current node, and status', async () => {
    const i18n = createInstance();
    await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
    const html = renderToStaticMarkup(
      createElement(
        I18nextProvider,
        { i18n },
        createElement(
          MemoryRouter,
          null,
          createElement(RunTable, {
            runs: [run({ id: 'run-9', name: 'nightly', currentNode: 'lint' })],
            onOpen: () => {},
          })
        )
      )
    );
    expect(html).toContain('nightly');
    expect(html).toContain('lint');
    expect(html).toContain('Running');
    // Static render cannot fire the click; the handler in the source names the route.
    expect(source('features/pipelines/PipelinesPage.tsx')).toContain(
      'encodeURIComponent(run.id)'
    );
  });
});

describe('run detail pieces', () => {
  test('a gate requires server-supplied details for the active human visit', () => {
    const gate = { runId: 'run-1', nodeId: 'n1', visitSequence: 3, question: 'Approve?', kind: 'yes_no' as const, options: [{ id: 'yes', label: 'Yes' }] };
    const activeHuman = run({
      status: 'waiting_human',
      nodes: [{ id: 'n1', kind: 'human', status: 'active', label: 'Approve the land?' }],
      gate,
    });
    expect(openGate(activeHuman)).toEqual(gate);
    expect(openGate({ ...activeHuman, gate: undefined })).toBeNull();
    expect(openGate({ ...activeHuman, status: 'running' })).toEqual(gate);
    expect(openGate({ ...activeHuman, status: 'succeeded' })).toBeNull();
    expect(openGate({ ...activeHuman, gate: { ...gate, nodeId: 'other' } })).toBeNull();
    expect(openGate({ ...activeHuman, gate: { ...gate, runId: 'other' } })).toBeNull();
    expect(openGate({ ...activeHuman, gate: { ...gate, visitSequence: 0 } })).toBeNull();
  });

  test('finished runs stop polling; loaded and unloaded runs keep it', () => {
    expect(runIsLive(null)).toBe(true);
    expect(runIsLive(run({ status: 'waiting_human' }))).toBe(true);
    expect(runIsLive(run({ status: 'succeeded' }))).toBe(false);
    expect(runIsLive(run({ status: 'failed' }))).toBe(false);
  });

  test('polling and gate answers refresh both the run and its graph', () => {
    const page = source('features/pipelines/RunDetailPage.tsx');
    expect(page).toContain('void run.refresh();\n      void graph.refresh();');
    expect(page).toContain('id && runIsLive(detail) ? 5000 : null');
    expect(page).toContain('Promise.all([run.refresh(), graph.refresh()])');
  });

  test('untrusted SVG is displayed as an image, never injected into the document', async () => {
    const i18n = createInstance();
    await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
    const svg = '<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(1)</script></svg>';
    const html = renderToStaticMarkup(
      createElement(I18nextProvider, { i18n }, createElement(RunGraph, { svg }))
    );
    expect(html).toContain('<img');
    expect(html).toContain('data:image/svg+xml,');
    expect(html).toContain('alt="Run graph"');
    expect(html).not.toContain('<svg');
    expect(html).not.toContain('<script>');
    expect(html).not.toContain(' onload=');
  });

  test('the timeline lists every node with its outcome', async () => {
    const i18n = createInstance();
    await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
    const html = renderToStaticMarkup(
      createElement(
        I18nextProvider,
        { i18n },
        createElement(NodeTimeline, {
          nodes: [
            { id: 'n0', kind: 'start', status: 'done', label: 'start' },
            { id: 'n1', kind: 'actor', status: 'active', label: 'lint' },
            { id: 'n2', kind: 'human', status: 'pending', label: 'approve', outcome: '' },
            { id: 'n3', kind: 'land', status: 'failed', label: 'land', outcome: 'merge conflict' },
          ],
        })
      )
    );
    for (const label of ['lint', 'approve', 'land', 'merge conflict', 'actor', 'human']) {
      expect(html).toContain(label);
    }
    expect(html).not.toContain('>&lt;</li>');
  });
});

describe('navigation and access', () => {
  test('routes and the sidebar entry exist under the operate group', () => {
    const routes = source('router/MainRoutes.tsx');
    expect(routes).toContain("path: '/pipelines', element: <PipelinesPage />");
    expect(routes).toContain("path: '/pipelines/:id', element: <RunDetailPage />");
    const layout = source('components/layout/MainLayout.tsx');
    expect(layout).toContain("path: '/pipelines'");
    expect(layout).toContain("labelKey: 'nav.pipelines'");
  });

  test('pipeline listing uses the selected project and accessible swarm sections', () => {
    const page = source('features/pipelines/PipelinesPage.tsx');
    expect(page).toContain('<ProjectScope');
    expect(page).toContain('<SwarmSections project={project}');
    expect(page).toContain('key={swarm.id}');
    expect(page).toContain('pipelinesApi.listRuns(swarmId)');
    expect(page).not.toContain('pipelinesApi.listRuns()');
  });

  test('ordinary users see pipelines; project member editing stays admin-only', () => {
    expect(visibleAccessPath('/pipelines', false)).toBe(true);
    expect(visibleAccessPath('/pipelines/run-1', false)).toBe(true);
    expect(visibleAccessPath('/projects/p/members', false)).toBe(false);
  });
});

describe('locales', () => {
  const locales = ['en', 'ru', 'zh-CN', 'zh-TW', 'vi', 'ko'];
  const flatten = (value: unknown, prefix = ''): Record<string, string> => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) {
      return prefix ? { [prefix]: String(value ?? '') } : {};
    }
    return Object.entries(value).reduce<Record<string, string>>((leaves, [key, child]) => {
      const path = prefix ? `${prefix}.${key}` : key;
      return { ...leaves, ...flatten(child, path) };
    }, {});
  };
  const tokens = (value: string): string[] => value.match(/\{\{[^}]+\}\}/g) ?? [];

  test('every locale covers the new pipelines keys with matching interpolations', () => {
    const englishKeys = Object.keys(flatten(en)).filter(
      (key) => key.startsWith('pipelines.') || key === 'nav.pipelines' || key === 'nav_meta.pipelines'
    );
    expect(englishKeys.length).toBeGreaterThan(0);
    for (const locale of locales) {
      const other = flatten(
        JSON.parse(readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8'))
      );
      const missing = englishKeys.filter((key) => !(key in other));
      const mismatched = englishKeys.filter((key) => key in other).filter(
        (key) => tokens(flatten(en)[key]).sort().join('|') !== tokens(other[key]).sort().join('|')
      );
      expect([locale, missing, mismatched]).toEqual([locale, [], []]);
    }
  });
});
