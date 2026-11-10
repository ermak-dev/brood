// FEATURE: The swarm page is the primary panel object (#215, epic #204).
// SPEC: /swarms/:id opens exactly that swarm; the page is titled by the
// swarm, not by the graph definition; known names of the swarm, project,
// owner and actors are shown instead of IDs, and the full swarm ID is a
// separate copy element; infrastructure and execution state are separate,
// and a stopped swarm reads differently from one waiting for an answer;
// the selected graph node drives the node details below the graph.
import { afterEach, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import type { AccessProject, AccessSwarm } from '@/services/brood/access';
import type { PipelineRun, RunGraph } from '@/services/brood/pipelines';
import {
  executionTone,
  hasStepTimes,
  nodeView,
  selectedNodeId,
  swarmExecution,
  swarmProjectName,
} from '@/features/swarms/swarmState';
import { SWARMS_PATH, swarmPath } from '@/features/swarms/routes';
import { accessApi } from '@/services/brood/access';
import { pipelinesApi } from '@/services/brood/pipelines';

const swarm = (over: Partial<AccessSwarm> = {}): AccessSwarm => ({
  id: 'swarm-0123456789abcdef',
  project_id: 'project-0123456789',
  owner_id: 'user-0123456789',
  owner_username: 'alice',
  role: 'owner',
  name: 'release-crew',
  atespace_name: 'brood-0123456789',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-02T09:00:00Z',
  ...over,
});

const run = (over: Partial<PipelineRun> = {}): PipelineRun => ({
  id: 'swarm-0123456789abcdef',
  name: 'swarm-architecture',
  status: 'running',
  swarmId: 'swarm-0123456789abcdef',
  swarmName: 'release-crew',
  projectId: 'project-0123456789',
  projectName: 'brood',
  owner: 'alice',
  startedAt: null,
  nodes: [],
  ...over,
});

const graph: RunGraph = {
  svg: '<svg xmlns="http://www.w3.org/2000/svg"></svg>',
  nodes: [
    { id: 'architecture', kind: 'actor', prompt: 'Propose the architecture' },
    { id: 'decision', kind: 'human', question: 'Approve the proposal?' },
    { id: 'done', kind: 'exit' },
  ],
};

describe('execution state', () => {
  test('a swarm without a graph is idle; the waiting wire name is the swarm state', () => {
    expect(swarmExecution(null)).toBe('idle');
    expect(swarmExecution(run({ status: 'waiting_human' }))).toBe('waiting');
    for (const status of ['idle', 'running', 'succeeded', 'failed', 'stopped'] as const) {
      expect(swarmExecution(run({ status }))).toBe(status);
    }
    // A newer server state passes through instead of turning into another one.
    expect(swarmExecution(run({ status: 'paused' as PipelineRun['status'] }))).toBe('paused');
  });

  test('stopped and waiting never share a tone; failure stays red', () => {
    expect(executionTone('waiting')).toBe('warning');
    expect(executionTone('stopped')).toBe('muted');
    expect(executionTone('failed')).toBe('danger');
    expect(executionTone('running')).toBe('success');
    expect(executionTone('succeeded')).toBe('success');
    expect(executionTone('idle')).toBe('muted');
    expect(executionTone('paused')).toBe('muted');
  });
});

describe('selected node', () => {
  const steps = run({
    currentNode: 'decision',
    nodes: [
      {
        id: 'architecture',
        kind: 'actor',
        label: 'architecture',
        status: 'failed',
        outcome: 'fail',
        actorName: 'architect-old',
        sequence: 1,
        startedAt: '2026-11-03T09:00:00Z',
        finishedAt: '2026-11-03T09:10:00Z',
      },
      {
        id: 'architecture',
        kind: 'actor',
        label: 'architecture',
        status: 'done',
        outcome: 'success',
        actorName: 'architect',
        sequence: 2,
        startedAt: '2026-11-03T10:00:00Z',
        finishedAt: '2026-11-03T10:20:00Z',
      },
      { id: 'decision', kind: 'human', label: 'decision', status: 'active', sequence: 3 },
    ],
  });

  test('defaults to the node the execution stands on and keeps a known choice', () => {
    expect(selectedNodeId(steps, graph, undefined)).toBe('decision');
    expect(selectedNodeId(steps, graph, 'done')).toBe('done');
    // A choice from another graph does not survive: the current node wins.
    expect(selectedNodeId(steps, graph, 'deleted')).toBe('decision');
    expect(selectedNodeId(null, null, 'deleted')).toBeUndefined();
    expect(selectedNodeId(run(), graph, undefined)).toBeUndefined();
  });

  test('the latest visit gives the outcome, actor and moments; the graph gives the instruction', () => {
    expect(nodeView(steps, graph, 'architecture')).toEqual({
      id: 'architecture',
      label: 'architecture',
      kind: 'actor',
      status: 'done',
      outcome: 'success',
      actorName: 'architect',
      startedAt: '2026-11-03T10:00:00Z',
      finishedAt: '2026-11-03T10:20:00Z',
      visits: 2,
      prompt: 'Propose the architecture',
      question: undefined,
    });
  });

  test('a node never visited is pending and still shows what the graph asks of it', () => {
    expect(nodeView(steps, graph, 'done')).toMatchObject({
      id: 'done',
      kind: 'exit',
      status: 'pending',
      visits: 0,
      outcome: undefined,
      actorName: undefined,
    });
    expect(nodeView(steps, graph, 'decision')).toMatchObject({
      kind: 'human',
      status: 'active',
      question: 'Approve the proposal?',
      visits: 1,
    });
    // Without the server node list the step kind is still known.
    expect(nodeView(steps, { svg: '' }, 'architecture')).toMatchObject({
      kind: 'actor',
      prompt: undefined,
    });
  });

  test('step time columns exist only when the server records moments', () => {
    expect(hasStepTimes(steps)).toBe(true);
    expect(
      hasStepTimes(run({ nodes: [{ id: 'a', kind: 'actor', label: 'a', status: 'done' }] }))
    ).toBe(false);
    expect(hasStepTimes(null)).toBe(false);
  });
});

describe('names instead of IDs', () => {
  const projects: AccessProject[] = [{ id: 'project-0123456789', name: 'brood' }];

  test('the project is named from the catalog, then from the run, the ID last', () => {
    expect(swarmProjectName(swarm(), projects, null)).toBe('brood');
    expect(swarmProjectName(swarm(), null, run({ projectName: 'from-run' }))).toBe('from-run');
    // The run of another project does not name this one.
    expect(
      swarmProjectName(swarm(), [], run({ projectId: 'other', projectName: 'other-name' }))
    ).toBe('project-0123456789');
  });
});

describe('addresses', () => {
  test('the swarm address escapes the ID and lives under the swarm list', () => {
    expect(SWARMS_PATH).toBe('/swarms');
    expect(swarmPath('a/b c')).toBe('/swarms/a%2Fb%20c');
  });
});

describe('API', () => {
  const originalFetch = globalThis.fetch;
  const calls: string[] = [];
  afterEach(() => {
    globalThis.fetch = originalFetch;
    calls.length = 0;
  });
  const reply = (body: unknown, status = 200) => {
    globalThis.fetch = (async (url: string) => {
      calls.push(url);
      return new Response(JSON.stringify(body), { status });
    }) as typeof fetch;
  };

  test('one swarm is read by its escaped ID', async () => {
    reply(swarm());
    expect((await accessApi.swarm('a/b')).name).toBe('release-crew');
    expect(calls).toEqual(['/api/swarms/a%2Fb']);
  });

  test('the execution of a swarm without a graph is null; other refusals stay errors', async () => {
    reply(run());
    expect((await pipelinesApi.getSwarmRun('a/b'))?.swarmName).toBe('release-crew');
    expect(calls).toEqual(['/api/pipeline-runs/a%2Fb']);
    reply({ detail: 'pipeline run not found' }, 404);
    expect(await pipelinesApi.getSwarmRun('a')).toBeNull();
    reply({ detail: 'internal error' }, 500);
    await expect(pipelinesApi.getSwarmRun('a')).rejects.toMatchObject({
      status: 500,
      message: 'internal error',
    });
  });
});

describe('locales', () => {
  const locales = ['en', 'ru', 'zh-CN', 'zh-TW', 'vi', 'ko'];
  const read = (locale: string) =>
    JSON.parse(
      readFileSync(new URL(`../src/i18n/locales/${locale}.json`, import.meta.url), 'utf8')
    ) as Record<string, unknown>;
  const flatten = (value: unknown, prefix = ''): Record<string, string> => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) {
      return prefix ? { [prefix]: String(value ?? '') } : {};
    }
    return Object.entries(value).reduce<Record<string, string>>((leaves, [key, child]) => {
      const path = prefix ? `${prefix}.${key}` : key;
      return { ...leaves, ...flatten(child, path) };
    }, {});
  };
  const tokens = (value: string) => (value.match(/\{\{[^}]+\}\}/g) ?? []).sort().join('|');
  const english = flatten(read('en'));
  const owned = (key: string) =>
    key.startsWith('swarm_page.') || key.startsWith('pipelines.statuses.');

  test('every locale carries every swarm page key with the same interpolations', () => {
    const keys = Object.keys(english).filter(owned);
    expect(keys.length).toBeGreaterThan(40);
    for (const locale of locales) {
      const translated = flatten(read(locale));
      const ownedThere = Object.keys(translated).filter(owned).sort();
      expect({ locale, keys: ownedThere }).toEqual({ locale, keys: [...keys].sort() });
      for (const key of keys) {
        expect({ locale, key, value: translated[key].trim().length > 0 }).toEqual({
          locale,
          key,
          value: true,
        });
        expect({ locale, key, tokens: tokens(translated[key]) }).toEqual({
          locale,
          key,
          tokens: tokens(english[key]),
        });
      }
    }
  });

  test('every key the swarm page names exists, including each state it can show', () => {
    const files = ['features/swarms/SwarmPage.tsx', 'features/access/SwarmManagement.tsx'];
    const used = files.flatMap((file) =>
      [
        ...readFileSync(new URL(`../src/${file}`, import.meta.url), 'utf8').matchAll(
          /'(swarm_page\.[a-z_]+)'/g
        ),
      ].map((match) => match[1])
    );
    expect(used.length).toBeGreaterThan(15);
    for (const key of used) expect({ key, known: key in english }).toEqual({ key, known: true });
    for (const state of ['idle', 'running', 'waiting', 'succeeded', 'failed', 'stopped']) {
      expect(english[`swarm_page.execution_states.${state}`]).toBeTruthy();
      expect(english[`swarm_page.execution_hints.${state}`]).toBeTruthy();
    }
    for (const state of [
      'unvisited',
      'pending',
      'active',
      'done',
      'failed',
      'skipped',
      'unconfirmed',
    ]) {
      expect(english[`swarm_page.node_states.${state}`]).toBeTruthy();
    }
  });
});
