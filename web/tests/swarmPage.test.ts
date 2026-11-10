// FEATURE: The swarm page is the primary panel object (#215, epic #204).
// SPEC: /swarms/:id opens exactly that swarm; the page is titled by the
// swarm, not by the graph definition; known names of the swarm, project,
// owner and actors are shown instead of IDs, and the full swarm ID is a
// separate copy element; infrastructure and execution state are separate,
// and a stopped swarm reads differently from one waiting for an answer;
// the selected graph node drives the node details below the graph. The page
// shows the goal of the current pass, and the step history splits into
// passes, each under its own goal (#242).
import { afterEach, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import type { AccessProject, AccessSwarm } from '@/services/brood/access';
import type { NodeStatus, PipelineRun, RunGraph } from '@/services/brood/pipelines';
import {
  executionTone,
  hasStepTimes,
  nodeView,
  passGoal,
  releaseCommand,
  runIsLive,
  selectedNodeId,
  shortCommit,
  stepMark,
  stepPasses,
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

  test('a finished execution stops polling; unread, running and waiting keep it', () => {
    expect(runIsLive(null)).toBe(true);
    expect(runIsLive(run({ status: 'running' }))).toBe(true);
    expect(runIsLive(run({ status: 'waiting_human' }))).toBe(true);
    for (const status of ['idle', 'succeeded', 'failed', 'stopped'] as const) {
      expect(runIsLive(run({ status }))).toBe(false);
    }
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

  test('the latest visit gives the reason, the confirmed commit and the report', () => {
    const sha = '0123456789abcdef0123456789abcdef01234567';
    const reported = run({
      currentNode: 'decision',
      nodes: [
        {
          id: 'architecture',
          kind: 'actor',
          label: 'architecture',
          status: 'refused',
          detail: 'actor architect is not connected to the relay; no instruction was sent',
          sequence: 1,
        },
        {
          id: 'architecture',
          kind: 'actor',
          label: 'architecture',
          status: 'done',
          outcome: 'success',
          commit: sha,
          report: 'Branch feat/x\nMR !12',
          sequence: 2,
        },
        { id: 'decision', kind: 'human', label: 'decision', status: 'active', sequence: 3 },
      ],
    });
    // The reason of an earlier visit does not stick to the later one.
    expect(nodeView(reported, graph, 'architecture')).toMatchObject({
      status: 'done',
      detail: undefined,
      commit: sha,
      report: 'Branch feat/x\nMR !12',
    });
    expect(nodeView(reported, graph, 'decision')).toMatchObject({
      detail: undefined,
      commit: undefined,
      report: undefined,
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

// The step states the server records (#212, docs/MODEL.md «Шаг актора»)
// and the names the run view gives a working or finished step.
const STEP_STATES = [
  'pending',
  'attesting',
  'delivering',
  'inflight',
  'waiting',
  'answered',
  'completed',
  'refused',
  'unconfirmed',
  'stopped',
] as const;
const VIEW_STATES = ['active', 'done', 'failed', 'skipped'] as const;

describe('actor step states (#212, #238)', () => {
  const actorStep = (status: string, sequence: number, id = 'architecture') => ({
    id,
    kind: 'actor' as const,
    label: id,
    status: status as NodeStatus,
    sequence,
  });

  test('every step state has its own mark; refused reads as a failure, unconfirmed never', () => {
    for (const state of ['attesting', 'delivering', 'inflight', 'waiting', 'active']) {
      expect({ state, mark: stepMark(state) }).toEqual({ state, mark: 'active' });
    }
    for (const state of ['answered', 'completed', 'done']) {
      expect({ state, mark: stepMark(state) }).toEqual({ state, mark: 'done' });
    }
    expect(stepMark('refused')).toBe('failed');
    expect(stepMark('failed')).toBe('failed');
    expect(stepMark('unconfirmed')).toBe('unconfirmed');
    expect(stepMark('stopped')).toBe('skipped');
    expect(stepMark('skipped')).toBe('skipped');
    expect(stepMark('pending')).toBe('pending');
    expect(stepMark('unvisited')).toBe('pending');
    // A state of a newer server is not dressed up as progress or failure.
    expect(stepMark('paused')).toBe('pending');
  });

  test('guidance belongs to the refused or unconfirmed step the execution stands on', () => {
    for (const state of ['refused', 'unconfirmed'] as const) {
      const stuck = run({ currentNode: 'architecture', nodes: [actorStep(state, 1)] });
      expect(nodeView(stuck, graph, 'architecture').guidance).toBe(state);
    }
    for (const state of STEP_STATES.filter((s) => s !== 'refused' && s !== 'unconfirmed')) {
      const step = run({ currentNode: 'architecture', nodes: [actorStep(state, 1)] });
      expect({ state, guidance: nodeView(step, graph, 'architecture').guidance }).toEqual({
        state,
        guidance: undefined,
      });
    }
    // A new pass already answered the refusal of an earlier one.
    const later = run({
      currentNode: 'decision',
      nodes: [actorStep('refused', 1), actorStep('pending', 2, 'decision')],
    });
    expect(nodeView(later, graph, 'architecture')).toMatchObject({
      status: 'refused',
      guidance: undefined,
    });
    expect(nodeView(null, graph, 'architecture').guidance).toBeUndefined();
  });

  test('the lease release names the step actor and the swarm; the operation stays a slot', () => {
    expect(releaseCommand('swarm-1', 'architect')).toBe(
      'bro dispatch architect --request-id <operation> --action release --swarm swarm-1'
    );
    // The actor of a step the server could not name is a slot too, not "undefined".
    expect(releaseCommand('swarm-1', undefined)).toBe(
      'bro dispatch <actor> --request-id <operation> --action release --swarm swarm-1'
    );
  });

  test('a commit is shown by its short form; a short value stays whole', () => {
    expect(shortCommit('0123456789abcdef0123456789abcdef01234567')).toBe('01234567');
    expect(shortCommit('abc')).toBe('abc');
  });
});

describe('passes', () => {
  const step = (sequence: number, pass?: number, goal?: string) => ({
    id: `node-${sequence}`,
    kind: 'actor' as const,
    label: `node-${sequence}`,
    status: 'done' as const,
    sequence,
    ...(pass === undefined ? {} : { pass }),
    ...(goal === undefined ? {} : { goal }),
  });

  test('steps split into passes in order, each with the goal its first step keeps', () => {
    const steps = [
      step(1, 1, 'Fix issue #12'),
      step(2, 1),
      step(3, 3, 'Fix issue #13\nand land it'),
      step(4, 3),
      step(5, 3),
    ];
    expect(stepPasses(steps)).toEqual([
      { pass: 1, number: 1, goal: 'Fix issue #12', steps: [steps[0], steps[1]] },
      { pass: 3, number: 2, goal: 'Fix issue #13\nand land it', steps: steps.slice(2) },
    ]);
  });

  test('a pass whose first step is not listed has no goal rather than a borrowed one', () => {
    const steps = [step(4, 3), step(5, 3)];
    expect(stepPasses(steps)).toEqual([{ pass: 3, number: 1, steps }]);
  });

  test('steps of a server without passes stay one list without a pass', () => {
    const steps = [step(1), step(2)];
    expect(stepPasses(steps)).toEqual([{ steps }]);
    expect(stepPasses([])).toEqual([]);
  });

  test('the goal of the current pass comes from the run, its pass from the last step', () => {
    const nodes = [step(1, 1, 'Fix issue #12'), step(2, 2, 'Fix issue #13'), step(3, 2)];
    expect(passGoal(run({ goal: 'Fix issue #13', nodes }))).toEqual({
      goal: 'Fix issue #13',
      pass: 2,
      number: 2,
    });
    // A run without passes on its steps still shows the goal it names.
    expect(passGoal(run({ goal: 'Fix issue #12', nodes: [step(1)] }))).toEqual({
      goal: 'Fix issue #12',
    });
  });

  test('a run without a goal, or with something other than text, has none', () => {
    expect(passGoal(null)).toBeNull();
    expect(passGoal(run())).toBeNull();
    expect(passGoal(run({ goal: '' }))).toBeNull();
    expect(passGoal(run({ goal: 12 as unknown as string }))).toBeNull();
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

  test('the graph of a swarm is read by its escaped ID', async () => {
    reply({ svg: '<svg></svg>' });
    expect(await pipelinesApi.getRunGraph('a/b')).toEqual({ svg: '<svg></svg>' });
    expect(calls).toEqual(['/api/pipeline-runs/a%2Fb/graph']);
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
  const owned = (key: string) => key.startsWith('swarm_page.');

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
    // Every state a step can be in reads in words, stopped included (#238).
    for (const state of ['unvisited', ...STEP_STATES, ...VIEW_STATES]) {
      expect({ state, word: Boolean(english[`swarm_page.node_states.${state}`]) }).toEqual({
        state,
        word: true,
      });
    }
    for (const state of ['refused', 'unconfirmed', 'release']) {
      expect(english[`swarm_page.step_guidance.${state}`]).toBeTruthy();
    }
    // Two states never share a word: the word carries the state, not the color.
    for (const locale of locales) {
      const translated = flatten(read(locale));
      const words = ['unvisited', ...STEP_STATES, ...VIEW_STATES].map(
        (state) => translated[`swarm_page.node_states.${state}`]
      );
      expect({ locale, distinct: new Set(words).size }).toEqual({ locale, distinct: words.length });
    }
  });
});
