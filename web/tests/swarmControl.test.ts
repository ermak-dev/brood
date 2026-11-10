// FEATURE: Start and stop the swarm from its page (#218, API #211).
// SPEC: The page offers exactly one execution action, and only to a user
// who manages the active swarm, in a state the server accepts it from: start
// for a swarm without a snapshot, start again on the pinned snapshot for a
// finished or stopped one, stop for a running or waiting one. An unknown,
// still loading or unreadable execution offers nothing. The first start
// sends the chosen catalog definition and the commit only when one is
// typed; a restart sends the snapshot's definition without a commit. The
// snapshot identity comes from the swarm, never from the catalog.
import { afterEach, describe, expect, test } from 'bun:test';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun, WorkflowDefinition } from '@/services/brood/pipelines';
import { executionApi, startBody } from '@/services/brood/execution';
import {
  executionAction,
  definitionChoice,
  restartDefinition,
  snapshotIdentity,
} from '@/features/swarms/swarmControl';

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

const revision = '0123456789abcdef0123456789abcdef';

const run = (over: Partial<PipelineRun> = {}): PipelineRun => ({
  id: 'swarm-0123456789abcdef',
  name: 'release',
  definition: { name: 'release', version: 3, revision },
  status: 'stopped',
  startedAt: null,
  nodes: [],
  ...over,
});

describe('execution action', () => {
  test('a swarm without a snapshot starts; a finished or stopped one starts again', () => {
    expect(executionAction(swarm(), false, null)).toBe('start');
    for (const status of ['idle', 'succeeded', 'failed', 'stopped'] as const) {
      expect({ status, action: executionAction(swarm(), false, run({ status })) }).toEqual({
        status,
        action: 'restart',
      });
    }
  });

  test('a running or waiting swarm stops and never offers a second start', () => {
    expect(executionAction(swarm(), false, run({ status: 'running' }))).toBe('stop');
    expect(executionAction(swarm(), false, run({ status: 'waiting_human' }))).toBe('stop');
  });

  test('only a user who manages the swarm gets an action', () => {
    expect(executionAction(swarm({ role: 'editor' }), false, null)).toBe('start');
    expect(executionAction(swarm({ role: 'viewer' }), false, null)).toBeNull();
    expect(executionAction(swarm({ role: 'viewer' }), false, run({ status: 'running' }))).toBe(
      null
    );
    // An administrator manages every swarm, whatever the grant says.
    expect(executionAction(swarm({ role: 'viewer' }), true, run({ status: 'running' }))).toBe(
      'stop'
    );
  });

  test('a swarm that is not active offers nothing, not a disabled ghost', () => {
    for (const state of ['provisioning', 'error', 'deleting']) {
      expect({ state, action: executionAction(swarm({ state }), true, null) }).toEqual({
        state,
        action: null,
      });
    }
  });

  test('an execution not read yet, or in a state this panel does not know, offers nothing', () => {
    expect(executionAction(swarm(), true, undefined)).toBeNull();
    expect(
      executionAction(swarm(), true, run({ status: 'paused' as PipelineRun['status'] }))
    ).toBeNull();
  });
});

describe('snapshot identity', () => {
  test('the identity is the one the swarm recorded', () => {
    expect(snapshotIdentity(run())).toEqual({ name: 'release', version: 3, revision });
    expect(snapshotIdentity(null)).toBeNull();
  });

  test('a run without a well formed identity has none rather than a made-up one', () => {
    expect(snapshotIdentity(run({ definition: undefined }))).toBeNull();
    expect(snapshotIdentity(run({ definition: { name: '', version: 3, revision } }))).toBeNull();
    expect(
      snapshotIdentity(run({ definition: { name: 'release', version: 0, revision } }))
    ).toBeNull();
    expect(
      snapshotIdentity(run({ definition: { name: 'release', version: 1.5, revision } }))
    ).toBeNull();
    expect(
      snapshotIdentity(run({ definition: { name: 'release', version: 3, revision: '' } }))
    ).toBeNull();
  });

  test('a restart names the snapshot definition, falling back to the graph name', () => {
    expect(restartDefinition(run({ name: 'graph-name' }))).toBe('release');
    expect(restartDefinition(run({ name: 'release', definition: undefined }))).toBe('release');
  });
});

describe('definition choice', () => {
  const definition = (name: string, version = 1): WorkflowDefinition => ({
    name,
    version,
    revision,
    publishedAt: null,
  });

  test('the choice must be in the catalog of this project', () => {
    const catalog = [definition('release'), definition('review')];
    expect(definitionChoice(catalog, 'review')?.name).toBe('review');
    // A name from another project or a deleted one is not a choice.
    expect(definitionChoice(catalog, 'foreign')).toBeUndefined();
    expect(definitionChoice(null, 'release')).toBeUndefined();
  });

  test('nothing is chosen for the operator unless the catalog has one definition', () => {
    expect(definitionChoice([definition('release'), definition('review')], '')).toBeUndefined();
    expect(definitionChoice([definition('release', 4)], '')).toEqual(definition('release', 4));
    expect(definitionChoice([], '')).toBeUndefined();
  });

  test('a choice follows the catalog: a newer version replaces the one listed before', () => {
    expect(definitionChoice([definition('release', 5)], 'release')?.version).toBe(5);
  });
});

describe('start body', () => {
  test('the commit goes only when typed; spaces around it are dropped', () => {
    expect(startBody('release', '')).toEqual({ definition: 'release' });
    expect(startBody('release', '   ')).toEqual({ definition: 'release' });
    expect(startBody('release', ` ${'a'.repeat(40)}\n`)).toEqual({
      definition: 'release',
      commit: 'a'.repeat(40),
    });
  });

  test('a short commit is sent as typed: the server names the reason, not the panel', () => {
    expect(startBody('release', 'main')).toEqual({ definition: 'release', commit: 'main' });
  });
});

describe('API', () => {
  const originalFetch = globalThis.fetch;
  const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
  const calls: Array<{ url: string; method: string; body: unknown; csrf: string | null }> = [];
  afterEach(() => {
    globalThis.fetch = originalFetch;
    if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
    else Reflect.deleteProperty(globalThis, 'document');
    calls.length = 0;
  });
  const reply = (body: unknown, status = 200) => {
    // Изменяющий запрос несёт csrf cookie обратно заголовком.
    Object.defineProperty(globalThis, 'document', {
      configurable: true,
      value: { cookie: 'csrf=token-1' },
    });
    globalThis.fetch = (async (url: string, init?: RequestInit) => {
      const headers = new Headers(init?.headers);
      calls.push({
        url,
        method: init?.method ?? 'GET',
        body: typeof init?.body === 'string' ? JSON.parse(init.body) : init?.body,
        csrf: headers.get('X-CSRFToken'),
      });
      return new Response(JSON.stringify(body), { status });
    }) as typeof fetch;
  };
  const executed = {
    swarmId: 'a/b',
    execution: 'waiting',
    definition: { name: 'release', version: 3, revision },
    source: { origin: 'https://git.example.org/team/repo.git', commit: 'a'.repeat(40) },
    step: { sequence: 1, nodeId: 'ask', state: 'waiting' },
  };

  test('start posts the definition and commit to the escaped swarm', async () => {
    reply(executed);
    expect((await executionApi.start('a/b', 'release', 'a'.repeat(40))).execution).toBe('waiting');
    expect(calls).toEqual([
      {
        url: '/api/swarms/a%2Fb/start',
        method: 'POST',
        body: { definition: 'release', commit: 'a'.repeat(40) },
        csrf: 'token-1',
      },
    ]);
  });

  test('stop posts to the escaped swarm without a body', async () => {
    reply({ ...executed, execution: 'stopped' });
    expect((await executionApi.stop('a/b')).execution).toBe('stopped');
    expect(calls).toEqual([
      { url: '/api/swarms/a%2Fb/stop', method: 'POST', body: undefined, csrf: 'token-1' },
    ]);
  });

  test('a refusal carries the server reason and status', async () => {
    reply({ detail: 'swarm source is invalid: the first start requires the commit to pin' }, 422);
    await expect(executionApi.start('a', 'release', '')).rejects.toMatchObject({
      status: 422,
      message: 'swarm source is invalid: the first start requires the commit to pin',
    });
    expect(calls[0]?.body).toEqual({ definition: 'release' });
  });
});
