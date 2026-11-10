// FEATURE: Bind swarm roles from the swarm page (#241, API #235, model #208).
// SPEC: The page reads the role view of the swarm: the snapshot's roles, or
// before the first start the roles of the definition chosen in the start
// dialog, or no graph at all. Each role has a state in words and its bound
// actor by name. A manager of the active swarm binds a role to a managed
// actor of this swarm with the incarnation the actor list showed, binds it
// again or unbinds it; while the execution runs or waits nothing changes,
// and a viewer only reads. Only actors the server would accept are offered.
// The start dialog names the graph roles that are not ready before the
// start. Every key of the role block exists in all six locales.
import { afterEach, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import type { AccessSwarm } from '@/services/brood/access';
import type { PipelineRun } from '@/services/brood/pipelines';
import {
  bindingBody,
  parseSwarmRoles,
  rolesApi,
  type GraphRole,
  type RoleActor,
  type SwarmRolesView,
} from '@/services/brood/roles';
import {
  actorChoice,
  actorsRefreshKey,
  bindableActors,
  roleControl,
  rolesDefinition,
  rolesRefreshKey,
  roleTone,
  unreadyRoles,
} from '@/features/swarms/roleState';

const swarm = (over: Partial<AccessSwarm> = {}): AccessSwarm => ({
  id: 'swarm-1',
  project_id: 'project-1',
  owner_id: 'user-1',
  owner_username: 'alice',
  role: 'owner',
  name: 'release-crew',
  atespace_name: 'brood-1',
  state: 'active',
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-02T09:00:00Z',
  ...over,
});

const revision = '0123456789abcdef0123456789abcdef';

const run = (over: Partial<PipelineRun> = {}): PipelineRun => ({
  id: 'swarm-1',
  name: 'release',
  definition: { name: 'release', version: 1, revision },
  status: 'stopped',
  startedAt: null,
  nodes: [],
  ...over,
});

const view = (over: Partial<SwarmRolesView> = {}): SwarmRolesView => ({
  swarmId: 'swarm-1',
  execution: 'idle',
  graph: { pinned: true, name: 'release', version: 1, revision },
  roles: [],
  ...over,
});

const role = (over: Partial<GraphRole> = {}): GraphRole => ({
  role: 'builder',
  inGraph: true,
  state: 'unbound',
  ...over,
});

const actor = (over: Partial<RoleActor> = {}): RoleActor => ({
  uid: 'uid-builder',
  name: 'builder-1',
  incarnation: 2,
  phase: 'running',
  desired: 'running',
  ...over,
});

type Call = { method: string; url: string; body: unknown; csrf?: string };
const originalFetch = globalThis.fetch;
const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
afterEach(() => {
  globalThis.fetch = originalFetch;
  if (originalDocument) Object.defineProperty(globalThis, 'document', originalDocument);
  else Reflect.deleteProperty(globalThis, 'document');
});

/** Сервер ролей: один ответ на всё, вызовы записаны как есть. */
function reply(body: unknown, status = 200): Call[] {
  // Изменяющий запрос несёт csrf cookie обратно заголовком.
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: { cookie: 'csrf=token-1' },
  });
  const calls: Call[] = [];
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    const call: Call = {
      method: init?.method ?? 'GET',
      url,
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
    };
    const csrf = new Headers(init?.headers).get('X-CSRFToken');
    if (csrf) call.csrf = csrf;
    calls.push(call);
    return new Response(JSON.stringify(body), { status });
  }) as typeof fetch;
  return calls;
}

const wire = {
  swarmId: 'swarm-1',
  execution: 'stopped',
  graph: { pinned: true, name: 'release', version: 1, revision },
  roles: [
    {
      role: 'builder',
      inGraph: true,
      state: 'ready',
      actorUid: 'uid-builder',
      actorName: 'builder-1',
      actorIncarnation: 2,
    },
    { role: 'old', inGraph: false, state: 'foreign', actorUid: 'uid-x', actorIncarnation: 1 },
    { role: 'reviewer', inGraph: true, state: 'unbound' },
  ],
};

describe('role view from the wire', () => {
  test('keeps every role with its state and bound actor', () => {
    expect(parseSwarmRoles(wire)).toEqual({
      swarmId: 'swarm-1',
      execution: 'stopped',
      graph: { pinned: true, name: 'release', version: 1, revision },
      roles: [
        {
          role: 'builder',
          inGraph: true,
          state: 'ready',
          actorUid: 'uid-builder',
          actorName: 'builder-1',
          actorIncarnation: 2,
        },
        { role: 'old', inGraph: false, state: 'foreign', actorUid: 'uid-x', actorIncarnation: 1 },
        { role: 'reviewer', inGraph: true, state: 'unbound' },
      ],
    });
  });

  test('a swarm without a graph has a null graph', () => {
    expect(parseSwarmRoles({ ...wire, graph: null, roles: [] }).graph).toBeNull();
  });

  test('a malformed reply fails loudly instead of losing a role', () => {
    for (const bad of [
      null,
      { ...wire, roles: null },
      { ...wire, execution: 7 },
      { ...wire, graph: { pinned: 'yes', name: 'release', version: 1, revision } },
      { ...wire, roles: [{ role: '', inGraph: true, state: 'ready' }] },
      { ...wire, roles: [{ role: 'builder', inGraph: 'true', state: 'ready' }] },
      { ...wire, roles: [{ role: 'builder', inGraph: true }] },
      {
        ...wire,
        roles: [{ role: 'builder', inGraph: true, state: 'ready', actorIncarnation: 1.5 }],
      },
    ]) {
      expect(() => parseSwarmRoles(bad)).toThrow('Invalid swarm roles response');
    }
  });
});

describe('role API', () => {
  test('reads the view of the snapshot, or of a catalog definition by name', async () => {
    const calls = reply(wire);
    await rolesApi.list('a/b');
    await rolesApi.list('swarm-1', 'release notes');
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'GET /api/swarms/a%2Fb/roles',
      'GET /api/swarms/swarm-1/roles?definition=release%20notes',
    ]);
  });

  test('binds exactly the seen actor: its UID and incarnation, nothing else', async () => {
    const calls = reply(wire);
    const answered = await rolesApi.bind('swarm-1', 'builder', actor());
    expect(calls).toEqual([
      {
        method: 'PUT',
        url: '/api/swarms/swarm-1/roles/builder',
        body: { actorUid: 'uid-builder', actorIncarnation: 2 },
        csrf: 'token-1',
      },
    ]);
    // The answer is the role view after the write, not a guess of it.
    expect(answered.roles[0].state).toBe('ready');
    expect(bindingBody(actor({ uid: 'u', incarnation: 7 }))).toEqual({
      actorUid: 'u',
      actorIncarnation: 7,
    });
  });

  test('unbinds by role and answers the view after the write', async () => {
    const calls = reply({ ...wire, roles: [] });
    expect((await rolesApi.unbind('swarm-1', 'builder')).roles).toEqual([]);
    expect(calls).toEqual([
      {
        method: 'DELETE',
        url: '/api/swarms/swarm-1/roles/builder',
        body: undefined,
        csrf: 'token-1',
      },
    ]);
  });

  test('a refusal carries the server reason', async () => {
    const reason =
      'swarm execution is running or waiting; stop the swarm before changing role bindings';
    reply({ detail: reason }, 409);
    await expect(rolesApi.bind('swarm-1', 'builder', actor())).rejects.toThrow(reason);
  });

  test('actors that can hold a role are the managed ones, with the generation as incarnation', async () => {
    const calls = reply({
      results: [
        {
          metadata: { name: 'builder-1', uid: 'uid-builder' },
          provider: 'proxmox',
          generation: '3',
          phase: 'running',
          desired: 'running',
        },
        { metadata: { name: 'native-1', uid: 'uid-native' }, status: { state: 'RUNNING' } },
      ],
    });
    expect(await rolesApi.actors('swarm-1')).toEqual([
      {
        uid: 'uid-builder',
        name: 'builder-1',
        incarnation: 3,
        phase: 'running',
        desired: 'running',
      },
    ]);
    expect(calls.map((c) => c.url)).toEqual(['/api/swarms/swarm-1/actors']);
  });

  test('a managed actor without a usable incarnation is not guessed at', async () => {
    for (const generation of ['0', '-1', 'x', '1.5', '', undefined]) {
      reply({
        results: [
          {
            metadata: { name: 'builder-1', uid: 'uid-builder' },
            provider: 'proxmox',
            generation,
            phase: 'running',
            desired: 'running',
          },
        ],
      });
      await expect(rolesApi.actors('swarm-1')).rejects.toThrow('Invalid swarm actor list');
    }
  });
});

describe('who may change bindings', () => {
  test('an owner, an editor or an administrator of an active swarm binds', () => {
    expect(roleControl(swarm(), false, view())).toBe('edit');
    expect(roleControl(swarm({ role: 'editor' }), false, view())).toBe('edit');
    expect(roleControl(swarm({ role: 'viewer' }), true, view())).toBe('edit');
  });

  test('a viewer and a swarm that is not active only read', () => {
    expect(roleControl(swarm({ role: 'viewer' }), false, view())).toBe('read');
    expect(roleControl(swarm({ state: 'provisioning' }), false, view())).toBe('read');
    expect(roleControl(swarm({ state: 'error' }), true, view())).toBe('read');
  });

  test('a running or waiting execution keeps every binding as it is', () => {
    expect(roleControl(swarm(), false, view({ execution: 'running' }))).toBe('busy');
    expect(roleControl(swarm(), true, view({ execution: 'waiting' }))).toBe('busy');
    for (const execution of ['idle', 'succeeded', 'failed', 'stopped']) {
      expect({ execution, control: roleControl(swarm(), false, view({ execution })) }).toEqual({
        execution,
        control: 'edit',
      });
    }
  });

  test('nothing is offered before the server said what the execution is', () => {
    expect(roleControl(swarm(), true, null)).toBe('read');
  });

  test('a viewer reads even a running swarm without a busy note', () => {
    expect(roleControl(swarm({ role: 'viewer' }), false, view({ execution: 'running' }))).toBe(
      'read'
    );
  });
});

describe('which graph the roles come from', () => {
  test('a swarm with a snapshot reads its own roles, whatever the dialog chose', () => {
    expect(rolesDefinition(run(), 'review')).toBe('');
  });

  test('a swarm without a snapshot reads the definition chosen in the start dialog', () => {
    expect(rolesDefinition(null, 'review')).toBe('review');
    expect(rolesDefinition(null, '')).toBe('');
  });

  test('an execution not read yet asks for no definition', () => {
    expect(rolesDefinition(undefined, 'review')).toBe('');
  });

  test('the view is read again when the execution or the snapshot changes', () => {
    const idle = rolesRefreshKey(null);
    const started = rolesRefreshKey(run({ status: 'running' }));
    expect(started).not.toBe(idle);
    expect(rolesRefreshKey(run({ status: 'stopped' }))).not.toBe(started);
    expect(rolesRefreshKey(run({ status: 'running', nodes: [] }))).toBe(started);
    expect(rolesRefreshKey(undefined)).not.toBe(idle);
  });
});

describe('when the actor list is read again', () => {
  test('a role that changed state or actor asks for the actors again; a quiet poll does not', () => {
    const before = view({ roles: [role({ state: 'ready', actorUid: 'a', actorIncarnation: 1 })] });
    const same = view({ roles: [role({ state: 'ready', actorUid: 'a', actorIncarnation: 1 })] });
    // The actor was recreated: the binding is stale and the listed incarnation is old.
    const stale = view({ roles: [role({ state: 'stale', actorUid: 'a', actorIncarnation: 1 })] });
    const moved = view({ roles: [role({ state: 'ready', actorUid: 'b', actorIncarnation: 1 })] });
    expect(actorsRefreshKey(same)).toBe(actorsRefreshKey(before));
    expect(actorsRefreshKey(stale)).not.toBe(actorsRefreshKey(before));
    expect(actorsRefreshKey(moved)).not.toBe(actorsRefreshKey(before));
    expect(actorsRefreshKey(null)).toBe('');
  });
});

describe('actors offered for a role', () => {
  test('only managed actors the server accepts: provisioning or running and meant to run', () => {
    const offered = bindableActors([
      actor({ uid: 'a', phase: 'running' }),
      actor({ uid: 'b', phase: 'provisioning' }),
      actor({ uid: 'c', phase: 'stopped', desired: 'stopped' }),
      actor({ uid: 'd', phase: 'running', desired: 'stopped' }),
      actor({ uid: 'e', phase: 'error' }),
      actor({ uid: 'f', phase: 'stopping', desired: 'deleted' }),
    ]);
    expect(offered.map((item) => item.uid)).toEqual(['a', 'b']);
    expect(bindableActors(null)).toEqual([]);
  });

  test('the choice is the operator pick, else the bound actor, else the only one', () => {
    const one = actor({ uid: 'a', name: 'one' });
    const two = actor({ uid: 'b', name: 'two' });
    expect(actorChoice([one, two], role(), 'b')).toBe(two);
    expect(actorChoice([one, two], role({ state: 'stale', actorUid: 'b' }), undefined)).toBe(two);
    expect(actorChoice([one], role(), undefined)).toBe(one);
    expect(actorChoice([one, two], role(), undefined)).toBeUndefined();
    // A pick that is no longer offered is not silently replaced by another actor.
    expect(actorChoice([one], role(), 'gone')).toBeUndefined();
  });
});

describe('start dialog note', () => {
  test('names the graph roles a start would refuse, not leftover bindings', () => {
    const roles = [
      role({ role: 'builder', state: 'ready' }),
      role({ role: 'old', inGraph: false, state: 'missing' }),
      role({ role: 'reviewer', state: 'unbound' }),
      role({ role: 'tester', state: 'stale' }),
    ];
    expect(unreadyRoles(view({ roles })).map((item) => item.role)).toEqual(['reviewer', 'tester']);
    expect(unreadyRoles(null)).toEqual([]);
  });
});

describe('state tone', () => {
  test('ready reads as success; unbound waits; a broken binding is a failure', () => {
    expect(roleTone('ready')).toBe('success');
    expect(roleTone('unbound')).toBe('warning');
    for (const state of ['stale', 'inactive', 'foreign', 'missing']) {
      expect({ state, tone: roleTone(state) }).toEqual({ state, tone: 'danger' });
    }
    expect(roleTone('newer-state')).toBe('muted');
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
  const owned = (key: string) => key.startsWith('swarm_roles.');

  test('every locale carries every role block key with the same interpolations', () => {
    const keys = Object.keys(english).filter(owned);
    expect(keys.length).toBeGreaterThan(30);
    for (const locale of locales) {
      const translated = flatten(read(locale));
      const ownedThere = Object.keys(translated).filter(owned).sort();
      expect({ locale, keys: ownedThere }).toEqual({ locale, keys: [...keys].sort() });
      for (const key of keys) {
        expect({ locale, key, filled: translated[key].trim().length > 0 }).toEqual({
          locale,
          key,
          filled: true,
        });
        expect({ locale, key, tokens: tokens(translated[key]) }).toEqual({
          locale,
          key,
          tokens: tokens(english[key]),
        });
      }
    }
  });

  test('every key the role block and the start dialog name exists, with each state', () => {
    const files = ['features/swarms/SwarmRoles.tsx', 'features/swarms/SwarmControls.tsx'];
    const used = files.flatMap((file) =>
      [
        ...readFileSync(new URL(`../src/${file}`, import.meta.url), 'utf8').matchAll(
          /'(swarm_roles\.[a-z_.]+)'/g
        ),
      ].map((match) => match[1])
    );
    expect(used.length).toBeGreaterThan(20);
    for (const key of used) expect({ key, known: key in english }).toEqual({ key, known: true });
    for (const state of ['ready', 'unbound', 'stale', 'inactive', 'foreign', 'missing']) {
      expect(english[`swarm_roles.states.${state}`]).toBeTruthy();
      expect(english[`swarm_roles.state_hints.${state}`]).toBeTruthy();
    }
    for (const phase of ['provisioning', 'running']) {
      expect(english[`swarm_roles.phases.${phase}`]).toBeTruthy();
    }
  });
});
