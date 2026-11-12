// FEATURE: The project overview is the start page (#197, epic #190).
// SPEC: The overview tells from the swarm list rows alone (#234, #266) what
// waits for the operator, what runs and what is stuck, without a request per
// swarm. A row without a summary is unknown, not zero. The swarms needing the
// operator and the running ones are always listed, the rest fill the overview
// up to its limit. A summary tile leads to the swarm where to act: one swarm
// to its page, several to the swarm list. The waiting tile and the menu counter
// of the swarm list decide by one rule (#269).
import { describe, expect, test } from 'bun:test';
import type { AccessSwarm } from '@/services/brood/access';
import { waitingCount } from '@/features/access/swarmActivity';
import {
  OVERVIEW_SWARMS,
  overviewSummary,
  overviewSwarms,
  projectDetailsPath,
  summaryTarget,
} from '@/features/overview/overview';

const row = (id: string, fields: Partial<AccessSwarm> = {}): AccessSwarm => ({
  id,
  project_id: 'p1',
  owner_id: 'u1',
  owner_username: 'alice',
  role: 'owner',
  name: `swarm-${id}`,
  atespace_name: `space-${id}`,
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-01T09:00:00Z',
  execution: 'idle',
  openRequests: 0,
  undeliveredAnswers: 0,
  ...fields,
});

const ids = (rows: AccessSwarm[]) => rows.map((swarm) => swarm.id);

describe('overview summary', () => {
  test('an empty project has nothing to wait for and nothing unknown', () => {
    expect(overviewSummary([])).toEqual({
      total: 0,
      unknown: 0,
      waiting: [],
      openRequests: 0,
      undelivered: [],
      undeliveredAnswers: 0,
      running: [],
      stuck: [],
    });
  });

  test('the operator is waited for by a waiting graph and by an open question of any origin', () => {
    const summary = overviewSummary([
      row('gate', { execution: 'waiting', openRequests: 1 }),
      row('actor-asks', { execution: 'running', openRequests: 2 }),
      row('quiet', { execution: 'running' }),
      row('cancelled-gate', { execution: 'waiting' }),
    ]);
    expect(ids(summary.waiting)).toEqual(['gate', 'actor-asks', 'cancelled-gate']);
    expect(summary.openRequests).toBe(3);
    expect(ids(summary.running)).toEqual(['actor-asks', 'quiet']);
  });

  test('undelivered answers, running and stuck runs are counted by their own rows', () => {
    const summary = overviewSummary([
      row('done', { execution: 'succeeded', undeliveredAnswers: 2 }),
      row('lost', { execution: 'stopped', undeliveredAnswers: 1 }),
      row('stuck', { execution: 'failed' }),
      row('busy', { execution: 'running' }),
      row('fresh'),
    ]);
    expect(ids(summary.undelivered ?? [])).toEqual(['done', 'lost']);
    expect(summary.undeliveredAnswers).toBe(3);
    expect(ids(summary.stuck)).toEqual(['stuck']);
    expect(ids(summary.running)).toEqual(['busy']);
    expect(summary.waiting).toEqual([]);
    expect(summary.total).toBe(5);
    expect(summary.unknown).toBe(0);
  });

  test('a row without a summary or with a broken one is unknown, not zero and not idle', () => {
    const summary = overviewSummary([
      row('old', { execution: undefined, openRequests: undefined, undeliveredAnswers: undefined }),
      row('broken', { execution: 'waiting', openRequests: -1 }),
      row('fraction', { execution: 'running', openRequests: 1.5 }),
      row('fine', { execution: 'waiting', openRequests: 1 }),
    ]);
    expect(summary.total).toBe(4);
    expect(summary.unknown).toBe(3);
    expect(ids(summary.waiting)).toEqual(['fine']);
    expect(summary.openRequests).toBe(1);
    expect(summary.running).toEqual([]);
  });

  test('a server that does not count undelivered answers leaves them unknown', () => {
    const summary = overviewSummary([
      row('a', { undeliveredAnswers: undefined }),
      row('b', { execution: 'running', undeliveredAnswers: undefined }),
    ]);
    expect(summary.undelivered).toBeNull();
    expect(summary.undeliveredAnswers).toBeNull();
    expect(summary.unknown).toBe(0);
    expect(ids(summary.running)).toEqual(['b']);
  });

  // #269: плитка «Ждут вас» и счётчик меню у «Swarms» решают одинаково о
  // каждом рое, а не только сходятся числом.
  test('the waiting tile and the menu counter decide every swarm alike', () => {
    const rows = [
      row('gate', { execution: 'waiting', openRequests: 1 }),
      row('actor-asks', { execution: 'running', openRequests: 2 }),
      row('cancelled-gate', { execution: 'waiting' }),
      row('undelivered', { execution: 'succeeded', undeliveredAnswers: 1 }),
      row('broken', { execution: 'waiting', openRequests: -1 }),
      row('old', { execution: undefined, openRequests: undefined }),
      row('quiet', { execution: 'running' }),
    ];
    const decide = (count: (rows: AccessSwarm[]) => number) =>
      Object.fromEntries(rows.map((swarm) => [swarm.id, count([swarm])]));
    const tile = decide((list) => overviewSummary(list).waiting.length);
    expect(decide(waitingCount)).toEqual(tile);
    expect(ids(overviewSummary(rows).waiting)).toEqual(['gate', 'actor-asks', 'cancelled-gate']);
    expect(waitingCount(rows)).toBe(3);
  });

  test('a broken undelivered count is not a count', () => {
    const summary = overviewSummary([
      row('a', { undeliveredAnswers: -2 }),
      row('b', { undeliveredAnswers: '1' as unknown as number }),
      row('c', { undeliveredAnswers: 1 }),
    ]);
    expect(ids(summary.undelivered ?? [])).toEqual(['c']);
    expect(summary.undeliveredAnswers).toBe(1);
  });
});

describe('where a summary tile leads', () => {
  test('nowhere without swarms, to the swarm page for one, to the list for several', () => {
    expect(summaryTarget([])).toBeUndefined();
    expect(summaryTarget([row('a/b c')])).toBe('/swarms/a%2Fb%20c');
    expect(summaryTarget([row('a'), row('b')])).toBe('/swarms');
  });

  test('project details carry the project in the address', () => {
    expect(projectDetailsPath('p1')).toBe('/projects/p1');
    expect(projectDetailsPath('p/1 ?')).toBe('/projects/p%2F1%20%3F');
  });
});

describe('which swarms the overview lists', () => {
  test('swarms needing the operator come first, then running ones, then the rest', () => {
    const { shown, hidden } = overviewSwarms([
      row('idle'),
      row('busy', { execution: 'running' }),
      row('done-undelivered', { execution: 'succeeded', undeliveredAnswers: 1 }),
      row('broken-infra', { state: 'error' }),
      row('gate', { execution: 'waiting', openRequests: 1 }),
      row('stuck', { execution: 'failed' }),
      row('provisioning', { state: 'provisioning' }),
      row('actor-asks', { execution: 'running', openRequests: 2 }),
    ], 10);
    expect(ids(shown)).toEqual([
      'gate',
      'stuck',
      'actor-asks',
      'broken-infra',
      'done-undelivered',
      'busy',
      'provisioning',
      'idle',
    ]);
    expect(hidden).toBe(0);
  });

  test('equal swarms keep the order of the server', () => {
    const { shown } = overviewSwarms([row('c'), row('a'), row('b')]);
    expect(ids(shown)).toEqual(['c', 'a', 'b']);
  });

  test('quiet swarms fill the overview up to its limit and the rest are counted', () => {
    const quiet = Array.from({ length: 10 }, (_, index) => row(`quiet-${index}`));
    const { shown, hidden } = overviewSwarms([
      ...quiet,
      row('busy', { execution: 'running' }),
      row('gate', { execution: 'waiting', openRequests: 1 }),
    ]);
    expect(shown).toHaveLength(OVERVIEW_SWARMS);
    expect(ids(shown).slice(0, 3)).toEqual(['gate', 'busy', 'quiet-0']);
    expect(hidden).toBe(12 - OVERVIEW_SWARMS);
  });

  test('a swarm needing the operator or running is never cut by the limit', () => {
    const urgent = Array.from({ length: OVERVIEW_SWARMS + 2 }, (_, index) =>
      row(`gate-${index}`, { execution: 'waiting', openRequests: 1 })
    );
    const { shown, hidden } = overviewSwarms([
      row('quiet'),
      ...urgent,
      row('busy', { execution: 'running' }),
    ]);
    expect(ids(shown)).toEqual([...ids(urgent), 'busy']);
    expect(hidden).toBe(1);
  });

  test('a swarm without a summary is listed after the known ones', () => {
    const { shown } = overviewSwarms([
      row('unknown', { execution: undefined, openRequests: undefined }),
      row('idle'),
    ]);
    expect(ids(shown)).toEqual(['idle', 'unknown']);
  });
});
