/**
 * brood: role bindings of a swarm (#208, API #235, page #241).
 *
 * GET /api/swarms/{id}/roles[?definition=NAME] answers the role view
 * {swarmId, execution, graph, roles} for any swarm role. The graph is the
 * swarm's snapshot; before the first start, the named definition of the
 * project catalog; without either there is none (null). Roles are the union
 * of the graph's roles and the stored bindings, by name; each carries the
 * verdict a start would give it by its actor: ready, unbound, stale,
 * inactive, foreign or missing. The name of an actor of another swarm is
 * never sent.
 *
 * PUT /api/swarms/{id}/roles/{role} takes exactly {actorUid,
 * actorIncarnation?}; DELETE /api/swarms/{id}/roles/{role} removes the
 * binding. Both answer the role view after the write and refuse with a fixed
 * reason ({"detail": ...}), for example while the execution runs or waits.
 *
 * A role binds to a managed actor of the swarm: GET /api/swarms/{id}/actors,
 * rows with provider proxmox, whose generation is the incarnation the
 * binding records. The panel sends the incarnation it showed, so the server
 * binds exactly the actor the operator saw.
 */

import { request } from './http';

/** The graph the roles come from: the pinned snapshot or a catalog definition. */
export interface RoleGraph {
  pinned: boolean;
  name: string;
  version: number;
  revision: string;
}

/** A role of the graph or of a stored binding. */
export interface GraphRole {
  role: string;
  inGraph: boolean;
  /** ready, unbound, stale, inactive, foreign or missing; a newer server may add more. */
  state: string;
  actorUid?: string;
  /** Only for an actor of this swarm. */
  actorName?: string;
  /** The incarnation the binding recorded. */
  actorIncarnation?: number;
}

export interface SwarmRolesView {
  swarmId: string;
  /** Swarm execution: idle, running, waiting, succeeded, failed, stopped. */
  execution: string;
  graph: RoleGraph | null;
  roles: GraphRole[];
}

/** A managed actor of the swarm, the kind a role binds to. */
export interface RoleActor {
  uid: string;
  name: string;
  incarnation: number;
  phase: string;
  desired: string;
}

/** Exactly what the bind route accepts. */
export interface RoleBinding {
  actorUid: string;
  actorIncarnation: number;
}

const INVALID = 'Invalid swarm roles response';
const INVALID_ACTORS = 'Invalid swarm actor list';

const isRecord = (value: unknown): value is Record<string, unknown> =>
  Boolean(value) && typeof value === 'object' && !Array.isArray(value);

const positive = (value: unknown): value is number =>
  Number.isSafeInteger(value) && (value as number) > 0;

function parseGraph(value: unknown): RoleGraph | null {
  if (value === null || value === undefined) return null;
  if (
    !isRecord(value) ||
    typeof value.pinned !== 'boolean' ||
    typeof value.name !== 'string' ||
    !positive(value.version) ||
    typeof value.revision !== 'string'
  ) {
    throw new Error(INVALID);
  }
  return {
    pinned: value.pinned,
    name: value.name,
    version: value.version,
    revision: value.revision,
  };
}

function parseRole(value: unknown): GraphRole {
  if (
    !isRecord(value) ||
    typeof value.role !== 'string' ||
    value.role === '' ||
    typeof value.inGraph !== 'boolean' ||
    typeof value.state !== 'string' ||
    value.state === ''
  ) {
    throw new Error(INVALID);
  }
  const parsed: GraphRole = { role: value.role, inGraph: value.inGraph, state: value.state };
  for (const key of ['actorUid', 'actorName'] as const) {
    const field = value[key];
    if (field === undefined || field === null || field === '') continue;
    if (typeof field !== 'string') throw new Error(INVALID);
    parsed[key] = field;
  }
  const incarnation = value.actorIncarnation;
  if (incarnation !== undefined && incarnation !== null) {
    if (!positive(incarnation)) throw new Error(INVALID);
    parsed.actorIncarnation = incarnation;
  }
  return parsed;
}

/** The role view from the wire: a malformed reply fails loudly instead of
 * losing a role or showing a made-up state. */
export function parseSwarmRoles(value: unknown): SwarmRolesView {
  if (
    !isRecord(value) ||
    typeof value.swarmId !== 'string' ||
    typeof value.execution !== 'string' ||
    !Array.isArray(value.roles)
  ) {
    throw new Error(INVALID);
  }
  return {
    swarmId: value.swarmId,
    execution: value.execution,
    graph: parseGraph(value.graph),
    roles: value.roles.map(parseRole),
  };
}

/** Managed actors of an actor list; native actors cannot hold a role. The
 * generation is a decimal string: one that is not a positive integer is
 * refused, not guessed at, since the binding records exactly it. */
export function managedActors(value: unknown): RoleActor[] {
  if (!isRecord(value) || !Array.isArray(value.results)) throw new Error(INVALID_ACTORS);
  const actors: RoleActor[] = [];
  for (const row of value.results) {
    if (!isRecord(row) || row.provider !== 'proxmox') continue;
    const metadata = isRecord(row.metadata) ? row.metadata : {};
    const generation = typeof row.generation === 'string' ? row.generation : '';
    const incarnation = /^[1-9][0-9]*$/.test(generation) ? Number(generation) : 0;
    if (
      !positive(incarnation) ||
      typeof metadata.uid !== 'string' ||
      metadata.uid === '' ||
      typeof metadata.name !== 'string'
    ) {
      throw new Error(INVALID_ACTORS);
    }
    actors.push({
      uid: metadata.uid,
      name: metadata.name,
      incarnation,
      phase: typeof row.phase === 'string' ? row.phase : '',
      desired: typeof row.desired === 'string' ? row.desired : '',
    });
  }
  return actors;
}

/** Body of a binding: the actor's UID and the incarnation the panel showed. */
export function bindingBody(actor: Pick<RoleActor, 'uid' | 'incarnation'>): RoleBinding {
  return { actorUid: actor.uid, actorIncarnation: actor.incarnation };
}

const segment = encodeURIComponent;
const swarmPath = (swarmId: string) => `/api/swarms/${segment(swarmId)}`;
const rolePath = (swarmId: string, role: string) => `${swarmPath(swarmId)}/roles/${segment(role)}`;

export const rolesApi = {
  /** The role view; a definition names the catalog graph before the first start. */
  list: async (swarmId: string, definition = ''): Promise<SwarmRolesView> => {
    const query = definition ? `?definition=${segment(definition)}` : '';
    return parseSwarmRoles(await request<unknown>('GET', `${swarmPath(swarmId)}/roles${query}`));
  },
  bind: async (swarmId: string, role: string, actor: RoleActor): Promise<SwarmRolesView> =>
    parseSwarmRoles(await request<unknown>('PUT', rolePath(swarmId, role), bindingBody(actor))),
  unbind: async (swarmId: string, role: string): Promise<SwarmRolesView> =>
    parseSwarmRoles(await request<unknown>('DELETE', rolePath(swarmId, role))),
  /** Managed actors of the swarm: the candidates for a role. */
  actors: async (swarmId: string): Promise<RoleActor[]> =>
    managedActors(await request<unknown>('GET', `${swarmPath(swarmId)}/actors`)),
};
