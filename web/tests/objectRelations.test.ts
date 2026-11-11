// FEATURE: Relations between objects open by a link (#195, epic #190).
// SPEC: An actor names its template, pool, worker and swarm; a template its
// actors and the pools they run on; a pool its workers; a swarm counts its
// actors, templates and members. A related object is a link only when its
// page would open it: a template listed in the swarm, a worker or pool for
// an administrator (those pages are admin-only), an actor whose panel the
// user may open. The worker of a native actor is status.workerAssignment.
import { describe, expect, test } from 'bun:test';
import type { Actor, ActorTemplate, Worker } from '@/services/brood/substrate';
import type { SwarmMember } from '@/services/brood/access';
import {
  actorPool,
  actorRelated,
  actorTemplate,
  actorWorker,
  poolWorkers,
  swarmCounts,
  templateActors,
  templatePools,
  workerPool,
} from '@/features/objects/relations';

const scope = { project: 'p1', swarm: 's1' };

const native = (name: string, template: string, assignment?: object): Actor => ({
  metadata: { name, uid: `uid-${name}` },
  actorTemplate: { atespace: 'space-s1', name: template },
  status: {
    state: assignment ? 'ACTOR_STATE_RUNNING' : 'ACTOR_STATE_SUSPENDED',
    ...(assignment ? { workerAssignment: assignment } : {}),
  },
});

const running = native('luna', 'claude', {
  worker: { name: 'worker-7' },
  workerNamespace: 'ate-system',
  workerPool: 'gvisor-pool',
  workerPod: 'gvisor-pool-5d8f-abc',
  nodeName: 'node-a',
});
const sleeping = native('sol', 'claude');

const managed: Actor = {
  metadata: { name: 'coder', uid: 'uid-coder' },
  provider: 'proxmox',
  actorTemplate: { atespace: 'space-s1', name: 'claude-proxmox-pve' },
  placement: { node: 'pve-a', ctid: 105, pool: 'pve' },
  phase: 'running',
};

const template = (name: string, uid = `t-${name}`): ActorTemplate => ({
  metadata: { name, uid },
});

describe('actor -> template', () => {
  test('a template listed in the swarm is a link to it in that swarm', () => {
    expect(actorTemplate(running, scope, [template('claude')])).toEqual({
      name: 'claude',
      ref: { kind: 'template', project: 'p1', swarm: 's1', name: 'claude' },
    });
  });

  test('a template that is not listed is named without a link', () => {
    expect(actorTemplate(running, scope, [template('other')])).toEqual({ name: 'claude' });
    expect(actorTemplate(running, scope, null)).toEqual({ name: 'claude' });
  });

  test('a managed actor leads to its virtual template; no template is no relation', () => {
    expect(actorTemplate(managed, scope, [template('claude-proxmox-pve')])?.ref).toEqual({
      kind: 'template',
      project: 'p1',
      swarm: 's1',
      name: 'claude-proxmox-pve',
    });
    expect(actorTemplate({ metadata: { name: 'bare' } }, scope, [])).toBeUndefined();
  });
});

describe('actor -> pool and worker', () => {
  test('a native actor is placed on the worker and pool of its workerAssignment', () => {
    expect(actorWorker(running, true)).toEqual({
      name: 'worker-7',
      ref: { kind: 'worker', name: 'worker-7' },
    });
    expect(actorPool(running, true)).toEqual({
      name: 'gvisor-pool',
      ref: { kind: 'pool', name: 'gvisor-pool' },
    });
  });

  test('a managed actor runs in a Proxmox pool and on no native worker', () => {
    expect(actorPool(managed, true)).toEqual({
      name: 'pve',
      ref: { kind: 'proxmoxPool', name: 'pve' },
    });
    expect(actorWorker(managed, true)).toBeUndefined();
  });

  test('an actor without a worker (suspended) has neither', () => {
    expect(actorWorker(sleeping, true)).toBeUndefined();
    expect(actorPool(sleeping, true)).toBeUndefined();
  });

  test('workers and pools are named, not linked, for a non-administrator', () => {
    expect(actorWorker(running, false)).toEqual({ name: 'worker-7' });
    expect(actorPool(running, false)).toEqual({ name: 'gvisor-pool' });
    expect(actorPool(managed, false)).toEqual({ name: 'pve' });
  });
});

describe('template -> actors and pools', () => {
  const claude = template('claude', 'tpl-uid');
  // ateapi names a template's golden actor by the template's uid.
  const golden = native('tpl-uid', 'claude');
  const other = native('vega', 'other');
  const zed = native('zed', 'claude', { worker: { name: 'w-2' }, workerPool: 'kata-pool' });

  test('the actors using a template, by name, without its golden actor', () => {
    const names = templateActors(claude, [zed, other, golden, running, sleeping]).map(
      (actor) => actor.metadata.name
    );
    expect(names).toEqual(['luna', 'sol', 'zed']);
  });

  test('the pools its actors run on, once each, by name', () => {
    expect(templatePools(claude, [zed, running, sleeping, other], true)).toEqual([
      { name: 'gvisor-pool', ref: { kind: 'pool', name: 'gvisor-pool' } },
      { name: 'kata-pool', ref: { kind: 'pool', name: 'kata-pool' } },
    ]);
    const twin = native('twin', 'claude', { worker: { name: 'w-3' }, workerPool: 'gvisor-pool' });
    expect(templatePools(claude, [running, twin], false)).toEqual([{ name: 'gvisor-pool' }]);
  });

  test('a managed template leads to the Proxmox pool of its actors', () => {
    expect(templatePools(template('claude-proxmox-pve'), [managed], true)).toEqual([
      { name: 'pve', ref: { kind: 'proxmoxPool', name: 'pve' } },
    ]);
    expect(templatePools(claude, [], true)).toEqual([]);
  });
});

describe('an actor as a related object', () => {
  test('is a link to its panel in its swarm when the user may open it', () => {
    expect(actorRelated(running, scope, true)).toEqual({
      name: 'luna',
      ref: { kind: 'actor', project: 'p1', swarm: 's1', name: 'luna' },
    });
    expect(actorRelated(running, scope, false)).toEqual({ name: 'luna' });
  });
});

describe('worker <-> pool', () => {
  const worker = (name: string, pool?: string): Worker => ({
    metadata: { name },
    ...(pool ? { workerPool: pool } : {}),
  });

  test('a worker belongs to its pool', () => {
    expect(workerPool(worker('w-1', 'gvisor-pool'))).toEqual({
      name: 'gvisor-pool',
      ref: { kind: 'pool', name: 'gvisor-pool' },
    });
    expect(workerPool(worker('w-0'))).toBeUndefined();
  });

  test('a pool lists its workers by name', () => {
    const workers = [worker('w-2', 'gvisor-pool'), worker('w-9', 'kata'), worker('w-1', 'gvisor-pool')];
    expect(poolWorkers('gvisor-pool', workers).map((w) => w.metadata.name)).toEqual(['w-1', 'w-2']);
    expect(poolWorkers('empty', workers)).toEqual([]);
  });
});

describe('counts per swarm', () => {
  const member = (username: string): SwarmMember => ({
    user_id: username,
    username,
    display_name: '',
    role: 'viewer',
    disabled: false,
  });

  test('each count is the length of the list its link opens', () => {
    expect(
      swarmCounts({
        actors: [running, sleeping, managed],
        templates: [template('claude')],
        members: [member('alice'), member('bob')],
      })
    ).toEqual({ actors: 3, templates: 1, members: 2 });
  });

  test('a list that was not read is unknown, not zero', () => {
    expect(swarmCounts({ actors: null, templates: [], members: null })).toEqual({
      actors: null,
      templates: 0,
      members: null,
    });
  });
});
