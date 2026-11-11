import { BADGE_TONES } from '@/features/access/state';
// FEATURE: Состояния загрузки доступа и защита от обновления прежнего проекта.
// SPEC: Ошибка сервера показана текстом с повтором; старый callback не грузит прежний контекст.
// IMPLEMENTATION: Общий LoadState и отсечение callback после cleanup.
// RESULT: IMPLEMENTED — SSR и регрессия старого refresh проходят.
// STATUS: DONE
//
// #255: список роёв несёт исполнение и число открытых вопросов, ждущие
// сверху; проекты -- число роёв и отметку выбранного; Capacity свёрнут без
// инвентаря; группы акторов -- одна плашка отказа.
import { expect, spyOn, test } from 'bun:test';
import * as React from 'react';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter } from 'react-router-dom';
import * as header from '@/hooks/useHeaderRefresh';
import { LoadState, useAccessResource } from '@/features/access/shared';
import {
  executionActivityTone,
  swarmActivityOf,
  swarmOrder,
  waitingCount,
} from '@/features/access/swarmActivity';
import { SwarmsTable } from '@/features/access/SwarmsPage';
import { ProjectsTable } from '@/features/access/ProjectsPage';
import { CapacitySection } from '@/features/access/ProjectDetailPage';
import { groupFailures } from '@/features/substrate/components/SwarmSections';
import type { AccessProject, AccessSwarm, ProjectDetail } from '@/services/brood/access';
import en from '@/i18n/locales/en.json';

const i18n = createInstance();
await i18n.init({ lng: 'en', resources: { en: { translation: en } } });
const render = (node: React.ReactNode) =>
  renderToStaticMarkup(
    createElement(I18nextProvider, { i18n }, createElement(MemoryRouter, null, node))
  );

test('renders server refusals accessibly and hides old table data', () => {
  const markup = renderToStaticMarkup(
    React.createElement(LoadState, {
      loading: false,
      error: 'membership revoked <reason>',
      retry: async () => {},
      children: 'stale swarm table',
    })
  );
  expect(markup).toContain('role="alert"');
  expect(markup).toContain('membership revoked &lt;reason&gt;');
  expect(markup).toContain('<button');
  expect(markup).not.toContain('stale swarm table');
});

test('an old mutation refresh cannot reload a previous project after scope cleanup', async () => {
  const cleanups: Array<() => void> = [];
  const calls: string[] = [];
  const state = spyOn(React, 'useState').mockImplementation(((value: unknown) => [
    value,
    () => {},
  ]) as typeof React.useState);
  const ref = spyOn(React, 'useRef').mockImplementation(((value: unknown) => ({
    current: value,
  })) as typeof React.useRef);
  const callback = spyOn(React, 'useCallback').mockImplementation((value) => value);
  const effect = spyOn(React, 'useEffect').mockImplementation((action) => {
    const cleanup = action();
    if (cleanup) cleanups.push(cleanup);
  });
  const register = spyOn(header, 'useHeaderRefresh').mockImplementation(() => {});
  try {
    const resource = useAccessResource(async () => {
      calls.push('old project');
      return [];
    });
    expect(calls).toEqual(['old project']);
    cleanups.forEach((cleanup) => cleanup());
    await resource.refresh();
    expect(calls).toEqual(['old project']);
  } finally {
    state.mockRestore();
    ref.mockRestore();
    callback.mockRestore();
    effect.mockRestore();
    register.mockRestore();
  }
});

const swarm = (id: string, over: Partial<AccessSwarm> = {}): AccessSwarm => ({
  id,
  project_id: 'project-1',
  owner_id: 'user-1',
  owner_username: 'alice',
  role: 'owner',
  name: id,
  atespace_name: `ate-${id}`,
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-02T09:00:00Z',
  ...over,
});

// Сводка исполнения (#234) -- поля строки списка роёв.
const summary = (execution: string, openRequests = 0): Partial<AccessSwarm> => ({
  execution,
  openRequests,
});

// Ждущие ответа -- сверху, среди них дольше всех ждёт тот, у кого вопросов
// больше; затем вставшие, идущие, остальные; без сводки -- в конце; при
// равенстве порядок сервера сохраняется.
test('swarmOrder puts waiting swarms first and keeps the server order on ties', () => {
  const rows = [
    swarm('done', summary('succeeded')),
    swarm('stuck', summary('failed')),
    swarm('one-question', summary('waiting', 1)),
    swarm('busy', summary('running')),
    swarm('idle', summary('idle')),
    swarm('unknown'),
    swarm('two-questions', summary('waiting', 2)),
    swarm('stopped', summary('stopped')),
    swarm('busy-2', summary('running')),
    swarm('broken', { state: 'error' }),
  ];
  const ordered = [...rows].sort(swarmOrder).map((row) => row.id);
  expect(ordered).toEqual([
    'two-questions',
    'one-question',
    'stuck',
    'busy',
    'busy-2',
    'idle',
    'done',
    'stopped',
    'unknown',
    'broken',
  ]);
});

// Сводка берётся только из строки сервера: без полей или с браком --
// «неизвестно», а не ноль вопросов и не «не запущен».
test('swarmActivityOf reads the row summary and refuses a malformed one', () => {
  expect(swarmActivityOf(swarm('asks', summary('waiting', 1)))).toEqual({
    execution: 'waiting',
    openRequests: 1,
  });
  expect(swarmActivityOf(swarm('fresh', summary('idle')))).toEqual({
    execution: 'idle',
    openRequests: 0,
  });
  expect(swarmActivityOf(swarm('bare'))).toBeUndefined();
  expect(swarmActivityOf(swarm('half', { execution: 'running' }))).toBeUndefined();
  expect(
    swarmActivityOf(swarm('fraction', { execution: 'running', openRequests: 1.5 }))
  ).toBeUndefined();
});

test('waitingCount counts the rows waiting for an answer', () => {
  const rows = [
    swarm('a', summary('waiting', 2)),
    swarm('b', summary('running', 1)),
    swarm('c', summary('waiting')),
    swarm('d'),
  ];
  expect(waitingCount(rows)).toBe(2);
  expect(waitingCount(null)).toBe(0);
});

// Тон исполнения — сразу тон бейджа из словаря #254 (access/state.ts).
test('execution tones name attention for waiting and problem for a failed pass', () => {
  expect(executionActivityTone('waiting')).toBe('attention');
  expect(executionActivityTone('running')).toBe('progress');
  expect(executionActivityTone('failed')).toBe('problem');
  for (const execution of ['idle', 'succeeded', 'stopped', 'later-state']) {
    expect(executionActivityTone(execution)).toBe('neutral');
  }
  for (const execution of ['waiting', 'running', 'failed', 'idle']) {
    expect(BADGE_TONES).toContain(executionActivityTone(execution));
  }
});

test('the swarm list shows execution and open questions, waiting swarms first', () => {
  const rows = [
    swarm('quiet', { state: 'active', ...summary('running') }),
    swarm('asks', { state: 'active', owner_username: 'bob', ...summary('waiting', 3) }),
    swarm('halted', { state: 'stopped' }),
    swarm('fresh', { state: 'provisioning', ...summary('idle') }),
  ];
  const markup = render(createElement(SwarmsTable, { swarms: rows }));
  const cells = (label: string) =>
    [...markup.matchAll(new RegExp(`data-label="${label}"[^>]*>(.*?)</td>`, 'g'))].map((m) =>
      m[1].replace(/<[^>]+>/g, '')
    );
  expect(markup).toContain('>Execution<');
  expect(markup).toContain('>Open questions<');
  // Сводка у каждого роя, не только у активного; строка без сводки --
  // прочерк и в конце, в порядке сервера.
  expect(cells('Name')).toEqual(['asks', 'quiet', 'fresh', 'halted']);
  expect(cells('Execution')).toEqual(['Waiting for answer', 'Running', 'Not started', '—']);
  expect(cells('Open questions')).toEqual(['3', '0', '0', '—']);
  expect(markup).not.toContain('…');
  // Инфраструктурное состояние stopped переведено, а не показано ключом.
  expect(cells('State')).toContain('Stopped');
  expect(markup).not.toContain('access.states.stopped');
  // Ждущий рой и его вопросы -- тоном внимания, идущий -- тоном работы.
  expect(markup).toContain('data-tone="attention">Waiting for answer<');
  expect(markup).toContain('data-tone="attention">3<');
  expect(markup).toContain('data-tone="progress">Running<');
});

test('the project list counts swarms and marks the selected project', () => {
  const projects: AccessProject[] = [
    { id: 'p-1', name: 'Alpha', swarmCount: 4 },
    { id: 'p-2', name: 'Beta', swarmCount: 0 },
    { id: 'p-3', name: 'Gamma' },
  ];
  const markup = render(
    createElement(ProjectsTable, {
      projects,
      selected: 'p-2',
      onOpen: () => {},
    })
  );
  const cells = (label: string) =>
    [...markup.matchAll(new RegExp(`data-label="${label}"[^>]*>(.*?)</td>`, 'g'))].map((m) =>
      m[1].replace(/<[^>]+>/g, '')
    );
  expect(markup).toContain('>Swarms<');
  // Число -- из строки сервера (#234); без него -- прочерк, не ноль.
  expect(cells('Swarms')).toEqual(['4', '0', '—']);
  expect(cells('Name')[1]).toContain('Beta');
  expect(cells('Name')[1]).toContain('Active project');
  expect(cells('Name')[0]).not.toContain('Active project');
});

const detail = (over: Partial<ProjectDetail>): ProjectDetail => ({
  id: 'p-1',
  name: 'Alpha',
  capacity: {
    worker_selector: {},
    max_swarms: null,
    max_actors: '8',
    max_cpu_millicores: null,
    max_memory_bytes: null,
  },
  usage: null,
  usage_available: false,
  ...over,
});

test('capacity is collapsed while the inventory is unavailable and open otherwise', () => {
  const collapsed = render(createElement(CapacitySection, { detail: detail({}) }));
  expect(collapsed).toContain('<details');
  expect(collapsed).not.toMatch(/<details[^>]* open/);
  expect(collapsed).toContain('<summary');
  expect(collapsed).toContain('Reservation inventory is unavailable');
  // Четыре «Unknown» не притворяются числами: колонка не моноширинная, и
  // EntityTable не даёт ячейке подсказку mono-значения.
  expect(collapsed).not.toContain('title="Unknown"');
  expect(collapsed).toContain('Unknown');
  const open = render(
    createElement(CapacitySection, {
      detail: detail({
        usage_available: true,
        usage: { swarms: '2', actors: '5', cpu_millicores: '1500', memory_bytes: '1024' },
      }),
    })
  );
  expect(open).toMatch(/<details[^>]* open/);
  expect(open).toContain('>2<');
  expect(open).not.toContain('Reservation inventory is unavailable');
});

// Один и тот же отказ Substrate у каждого роя -- одна плашка с именами роёв,
// разные отказы -- по плашке на причину.
test('groupFailures folds identical refusals of several swarms into one notice', () => {
  expect(
    groupFailures([
      { name: 'alpha', error: 'substrate unreachable' },
      { name: 'beta', error: 'substrate unreachable' },
      { name: 'gamma', error: 'forbidden' },
    ])
  ).toEqual([
    { error: 'substrate unreachable', names: ['alpha', 'beta'] },
    { error: 'forbidden', names: ['gamma'] },
  ]);
  expect(groupFailures([])).toEqual([]);
});
