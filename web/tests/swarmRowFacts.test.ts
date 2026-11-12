// FEATURE: The swarm row names its workflow definition and its counts (#268,
// epic #190).
// SPEC: The row of GET /api/projects/{id}/swarms carries `definition`
// {name, version, revision} (absent before the first start), `memberCount`
// (the length of the swarm member list) and `managedActorCount` (managed
// actors only: native Substrate actors are not counted). The swarm list shows
// them without a request per swarm: the definition by name and version, the
// name linked to its entry in the project catalog, the members, and the
// managed actors labelled as managed actors, never as all the actors of the
// swarm. A row of an older server, without these fields, renders as before; a
// malformed field is lost alone, not the rest of the row.
import { afterAll, describe, expect, test } from 'bun:test';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { readFileSync } from 'node:fs';
import { createInstance } from 'i18next';
import { I18nextProvider } from 'react-i18next';
import { MemoryRouter, useLocation } from 'react-router-dom';
import type { AccessSwarm } from '@/services/brood/access';
import { factColumns, swarmFactsOf } from '@/features/access/swarmFacts';
import { SwarmsTable } from '@/features/access/SwarmsPage';
import { formatDateTimeValue } from '@/utils/format';
import { installTestDOM } from './helpers/dom';
import en from '@/i18n/locales/en.json';

const restoreDOM = installTestDOM();
afterAll(restoreDOM);

const i18n = createInstance();
await i18n.init({
  lng: 'en',
  resources: { en: { translation: en } },
  interpolation: { escapeValue: false },
});

const revision = '0123456789abcdef0123456789abcdef';
const definition = { name: 'review', version: 2, revision };

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
  execution: 'idle',
  openRequests: 0,
  ...over,
});

// Поля #268 строки нового сервера.
const facts: Partial<AccessSwarm> = { definition, memberCount: 3, managedActorCount: 2 };

/** Значение поля провода, которое тип строки не допускает: брак сервера. */
const odd = (value: unknown) => value as never;

describe('swarmFactsOf', () => {
  test('reads the definition and both counts of the row', () => {
    expect(swarmFactsOf(swarm('started', facts))).toEqual({
      definition,
      members: 3,
      managedActors: 2,
    });
  });

  test('a row of an older server has no facts, not zeros', () => {
    expect(swarmFactsOf(swarm('old'))).toEqual({});
  });

  test('a swarm never started has its counts and no definition; zero is a count', () => {
    expect(swarmFactsOf(swarm('fresh', { memberCount: 1, managedActorCount: 0 }))).toEqual({
      members: 1,
      managedActors: 0,
    });
  });

  test('a malformed field is lost alone', () => {
    for (const bad of [-1, 1.5, '3', null]) {
      expect(swarmFactsOf(swarm('s', { ...facts, memberCount: odd(bad) }))).toEqual({
        definition,
        managedActors: 2,
      });
      expect(swarmFactsOf(swarm('s', { ...facts, managedActorCount: odd(bad) }))).toEqual({
        definition,
        members: 3,
      });
    }
    for (const bad of [
      { ...definition, name: '' },
      { ...definition, version: 0 },
      { ...definition, version: 1.5 },
      { ...definition, revision: '' },
      'review',
      null,
    ]) {
      expect(swarmFactsOf(swarm('s', { ...facts, definition: odd(bad) }))).toEqual({
        members: 3,
        managedActors: 2,
      });
    }
  });

  test('the definition keeps only its identity', () => {
    const extra = { ...definition, publishedAt: '2026-11-05T10:00:00Z' };
    expect(swarmFactsOf(swarm('s', { definition: odd(extra) })).definition).toEqual(definition);
  });
});

describe('factColumns', () => {
  test('a column is shown when any row carries its field', () => {
    expect(
      factColumns([swarm('old'), swarm('fresh', { memberCount: 1, managedActorCount: 0 })])
    ).toEqual({ definition: false, members: true, managedActors: true });
    expect(factColumns([swarm('old'), swarm('started', facts)])).toEqual({
      definition: true,
      members: true,
      managedActors: true,
    });
  });

  test('rows of an older server show no new column', () => {
    expect(factColumns([swarm('a'), swarm('b')])).toEqual({
      definition: false,
      members: false,
      managedActors: false,
    });
    expect(factColumns([])).toEqual({ definition: false, members: false, managedActors: false });
  });
});

const render = (rows: AccessSwarm[]) =>
  renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(MemoryRouter, null, createElement(SwarmsTable, { swarms: rows }))
    )
  );
const headers = (markup: string) =>
  [...markup.matchAll(/<th[^>]*>(.*?)<\/th>/g)]
    .map((match) => match[1].replace(/<[^>]+>/g, ''))
    .filter(Boolean);
const cells = (markup: string, label: string) =>
  [...markup.matchAll(new RegExp(`data-label="${label}"[^>]*>(.*?)</td>`, 'g'))].map(
    (match) => match[1]
  );
const text = (html: string) => html.replace(/<[^>]+>/g, '');

describe('the swarm list', () => {
  test('names the workflow, the members and the managed actors of each row', () => {
    const markup = render([
      swarm('started', { execution: 'running', ...facts }),
      swarm('fresh', { memberCount: 1, managedActorCount: 0 }),
    ]);
    expect(headers(markup)).toEqual([
      'Name',
      'State',
      'Execution',
      'Open questions',
      'Workflow',
      'Members',
      'Managed actors',
      'Role',
      'Owner',
    ]);
    const [started, fresh] = cells(markup, 'Workflow');
    // Имя ведёт к определению в каталоге проекта (#270), версия -- снимка.
    expect(started).toMatch(
      /<a [^>]*href="\/projects\/project-1\?workflow=review"[^>]*>review<\/a>/
    );
    expect(text(started)).toBe('review v2');
    expect(started).toContain(`title="${en.swarm_page.snapshot.hint}"`);
    // Ячейка -- один элемент: на телефоне ячейка -- сетка «подпись -- значение»,
    // и версия, отдельный её потомок, ушла бы на свою строку без подписи.
    const cell = document.createElement('td');
    cell.innerHTML = started;
    expect(cell.childNodes.length).toBe(1);
    // Не запущенный рой определения не несёт: прочерк, а не ссылка.
    expect(text(fresh)).toBe('—');
    expect(fresh).not.toContain('<a ');
    expect(cells(markup, 'Members').map(text)).toEqual(['3', '1']);
    expect(cells(markup, 'Managed actors').map(text)).toEqual(['2', '0']);
    // Число managed-акторов объясняет себя: native-акторов в нём нет.
    expect(cells(markup, 'Managed actors')[0]).toContain(
      `title="${en.access.managed_actors_hint}"`
    );
    // Число -- не «все акторы роя»: такой колонки нет.
    expect(headers(markup)).not.toContain('Actors');
  });

  test('rows of an older server render the columns they rendered before', () => {
    const markup = render([
      swarm('quiet', { execution: 'running' }),
      swarm('asks', { execution: 'waiting', openRequests: 3 }),
    ]);
    expect(headers(markup)).toEqual([
      'Name',
      'State',
      'Execution',
      'Open questions',
      'Role',
      'Owner',
    ]);
    expect(markup).not.toContain('workflow=');
  });

  test('a row without a field shows a dash where other rows have it', () => {
    const markup = render([swarm('started', facts), swarm('old', { execution: 'succeeded' })]);
    expect(cells(markup, 'Workflow').map(text)).toEqual(['review v2', '—']);
    expect(cells(markup, 'Members').map(text)).toEqual(['3', '—']);
    expect(cells(markup, 'Managed actors').map(text)).toEqual(['2', '—']);
  });
});

// #268: строка несёт момент последнего исполнения (`executionAt`); список
// говорит рядом с исполнением, сколько с него прошло, а точный момент -- в
// подсказке. Строка без момента (старый сервер, рой без прохода) -- прежняя
// ячейка исполнения, без «только что» и без нуля.
describe('the moment of the execution in the swarm list', () => {
  const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

  test('says how long ago the execution changed, the exact moment in a tooltip', () => {
    const executionAt = ago(5 * 60_000 + 10_000);
    const markup = render([
      swarm('busy', { execution: 'running', executionAt }),
      swarm('fresh', { execution: 'stopped', executionAt: ago(5_000) }),
    ]);
    const [busy, fresh] = cells(markup, 'Execution');
    expect(text(busy)).toBe('Running5 minutes ago');
    const moment = Date.parse(executionAt);
    // Имена атрибутов HTML не различают регистр; React пишет dateTime как есть.
    expect(busy.toLowerCase()).toContain(
      `<time datetime="${new Date(moment).toISOString().toLowerCase()}"`
    );
    const absolute = formatDateTimeValue(moment, 'en');
    expect(busy).toContain(`title="${i18n.t('access.execution_at', { time: absolute })}"`);
    // Ячейка -- один элемент: на телефоне ячейка -- сетка «подпись -- значение».
    const cell = document.createElement('td');
    cell.innerHTML = busy;
    expect(cell.childNodes.length).toBe(1);
    expect(text(fresh)).toBe(`Stopped${en.access.execution_just_now}`);
  });

  test('a row without a moment keeps the execution cell it had', () => {
    const before = render([swarm('done', { execution: 'succeeded' })]);
    const after = render([
      swarm('done', { execution: 'succeeded' }),
      swarm('dated', { execution: 'running', executionAt: ago(60_000) }),
    ]);
    // Идущий -- выше завершённого (swarmOrder).
    const [dated, done] = cells(after, 'Execution');
    expect(done).toBe(cells(before, 'Execution')[0]);
    expect(done).not.toContain('<time');
    expect(text(dated)).toBe('Running1 minute ago');
  });

  test('a malformed moment is not shown and does not hide the execution', () => {
    const markup = render([swarm('odd', { execution: 'failed', executionAt: 'yesterday' })]);
    const [cell] = cells(markup, 'Execution');
    expect(text(cell)).toBe('Stuck');
    expect(cell).not.toContain('<time');
  });
});

function Address() {
  const { pathname, search } = useLocation();
  return createElement('output', { 'data-location': `${pathname}${search}` });
}

describe('the workflow link in a swarm row', () => {
  test('opens the catalog entry, not the swarm page of its row', async () => {
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () =>
      root.render(
        createElement(
          I18nextProvider,
          { i18n },
          createElement(
            MemoryRouter,
            { initialEntries: ['/swarms'] },
            createElement(SwarmsTable, { swarms: [swarm('started', facts)] }),
            createElement(Address)
          )
        )
      )
    );
    const location = () => container.querySelector('output')?.getAttribute('data-location');
    const workflow = container.querySelector<HTMLAnchorElement>(
      'a[href="/projects/project-1?workflow=review"]'
    );
    expect(workflow).not.toBeNull();
    await act(async () => workflow?.click());
    expect(location()).toBe('/projects/project-1?workflow=review');
    // Остальная строка по-прежнему ведёт на страницу роя.
    await act(async () =>
      container.querySelector<HTMLElement>('td[data-label="Members"]')?.click()
    );
    expect(location()).toBe('/swarms/started');
    await act(async () => root.unmount());
    container.remove();
  });
});

describe('locales', () => {
  const read = (locale: string) =>
    JSON.parse(
      readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
    ) as {
      access: Record<string, unknown>;
      project_overview: Record<string, unknown>;
    };

  test('every locale names the workflow, the members and the managed actors', () => {
    for (const locale of ['en', 'ru', 'zh-CN', 'zh-TW', 'vi', 'ko']) {
      const { access, project_overview: overview } = read(locale);
      for (const key of ['workflow', 'swarm_members', 'managed_actors', 'managed_actors_hint']) {
        const value = access[key];
        expect([locale, key, typeof value === 'string' && value.trim() !== '']).toEqual([
          locale,
          key,
          true,
        ]);
      }
      for (const key of ['swarm_members', 'managed_actors']) {
        expect([locale, key, String(overview[key] ?? '')]).toEqual([
          locale,
          key,
          expect.stringContaining('{{count}}'),
        ]);
      }
    }
  });

  test('every locale says when the execution changed', () => {
    for (const locale of ['en', 'ru', 'zh-CN', 'zh-TW', 'vi', 'ko']) {
      const { access } = read(locale);
      const justNow = access.execution_just_now;
      expect([locale, typeof justNow === 'string' && justNow.trim() !== '']).toEqual([
        locale,
        true,
      ]);
      expect([locale, String(access.execution_at ?? '')]).toEqual([
        locale,
        expect.stringContaining('{{time}}'),
      ]);
    }
  });
});
