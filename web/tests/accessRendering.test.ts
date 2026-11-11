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
  ACTIVITY_LIMIT,
  activityTargets,
  executionActivityTone,
  loadSwarmActivities,
  swarmActivityOf,
  swarmOrder,
  type SwarmActivity,
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

const activity = (execution: string, openRequests = 0): SwarmActivity => ({
  execution,
  openRequests,
});

// Ждущие ответа -- сверху, среди них дольше всех ждёт тот, у кого вопросов
// больше; затем вставшие, идущие, остальные; неизвестное -- в конце; при
// равенстве порядок сервера сохраняется.
test('swarmOrder puts waiting swarms first and keeps the server order on ties', () => {
  const rows = [
    swarm('done'),
    swarm('stuck'),
    swarm('one-question'),
    swarm('busy'),
    swarm('idle'),
    swarm('unknown'),
    swarm('two-questions'),
    swarm('stopped'),
    swarm('busy-2'),
    swarm('broken', { state: 'error' }),
  ];
  const activities: Record<string, SwarmActivity | null> = {
    done: activity('succeeded'),
    stuck: activity('failed'),
    'one-question': activity('waiting', 1),
    busy: activity('running'),
    idle: activity('idle'),
    unknown: null,
    'two-questions': activity('waiting', 2),
    stopped: activity('stopped'),
    'busy-2': activity('running'),
  };
  const ordered = [...rows].sort(swarmOrder(activities)).map((row) => row.id);
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

// Пока сводки #234 нет, панель собирает исполнение сама и только для
// активных роёв, не больше предела; рой со сводкой сервера в опрос не идёт.
test('activityTargets takes active swarms without a server summary, up to the limit', () => {
  const rows = [
    swarm('a'),
    swarm('b', { state: 'provisioning' }),
    swarm('c', { execution: 'waiting', openRequests: 1 }),
    ...Array.from({ length: ACTIVITY_LIMIT + 3 }, (_, index) => swarm(`active-${index}`)),
  ];
  const targets = activityTargets(rows).map((row) => row.id);
  expect(targets.length).toBe(ACTIVITY_LIMIT);
  expect(targets[0]).toBe('a');
  expect(targets).not.toContain('b');
  expect(targets).not.toContain('c');
  expect(ACTIVITY_LIMIT).toBe(20);
  expect(swarmActivityOf(rows[2])).toEqual({ execution: 'waiting', openRequests: 1 });
  expect(swarmActivityOf(rows[0])).toBeUndefined();
});

// Сбор по старым маршрутам: исполнение -- из прогона (404 -- не запущен),
// вопросы -- открытые запросы; отказ одного роя не роняет остальные.
test('loadSwarmActivities aggregates the run and the open requests per swarm', async () => {
  const calls: string[] = [];
  const result = await loadSwarmActivities([swarm('waits'), swarm('fresh'), swarm('down')], {
    run: async (id) => {
      calls.push(`run:${id}`);
      if (id === 'down') throw new Error('unavailable');
      return id === 'waits' ? { status: 'waiting_human' } : null;
    },
    requests: async (id) => {
      calls.push(`requests:${id}`);
      return id === 'waits'
        ? [{ state: 'open' }, { state: 'answered' }, { state: 'open' }]
        : [];
    },
  });
  expect(result).toEqual({
    waits: { execution: 'waiting', openRequests: 2 },
    fresh: { execution: 'idle', openRequests: 0 },
    down: null,
  });
  expect(calls.sort()).toEqual(
    ['run:waits', 'run:fresh', 'run:down', 'requests:waits', 'requests:fresh', 'requests:down'].sort()
  );
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
    swarm('quiet', { state: 'active' }),
    swarm('asks', { state: 'active', owner_username: 'bob' }),
    swarm('halted', { state: 'stopped' }),
    swarm('pending-row', { state: 'active' }),
  ];
  const markup = render(
    createElement(SwarmsTable, {
      swarms: rows,
      activities: {
        quiet: activity('running'),
        asks: activity('waiting', 3),
        halted: null,
      },
    })
  );
  const cells = (label: string) =>
    [...markup.matchAll(new RegExp(`data-label="${label}"[^>]*>(.*?)</td>`, 'g'))].map((m) =>
      m[1].replace(/<[^>]+>/g, '')
    );
  expect(markup).toContain('>Execution<');
  expect(markup).toContain('>Open questions<');
  // Без сводки (остановленный рой, ещё не опрошенный) -- в конце, в порядке сервера.
  expect(cells('Name')).toEqual(['asks', 'quiet', 'halted', 'pending-row']);
  expect(cells('Execution')).toEqual(['Waiting for answer', 'Running', '—', '…']);
  expect(cells('Open questions')).toEqual(['3', '0', '—', '…']);
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
    { id: 'p-1', name: 'Alpha' },
    { id: 'p-2', name: 'Beta' },
    { id: 'p-3', name: 'Gamma' },
  ];
  const markup = render(
    createElement(ProjectsTable, {
      projects,
      counts: { 'p-1': 4, 'p-2': null },
      selected: 'p-2',
      onOpen: () => {},
    })
  );
  const cells = (label: string) =>
    [...markup.matchAll(new RegExp(`data-label="${label}"[^>]*>(.*?)</td>`, 'g'))].map((m) =>
      m[1].replace(/<[^>]+>/g, '')
    );
  expect(markup).toContain('>Swarms<');
  expect(cells('Swarms')).toEqual(['4', '—', '…']);
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
