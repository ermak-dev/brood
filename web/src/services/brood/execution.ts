/**
 * brood: start and stop of a swarm's graph (#211, page #218, goal #242).
 *
 * POST /api/swarms/{id}/start takes exactly {definition, goal, commit?}: the
 * first start names a definition of the catalog of the swarm's project and
 * may name the full commit of the project repository to pin; a restart names
 * the pinned definition and leaves the commit out. Every start, the first
 * and each next one, carries the goal of its pass: the server keeps it
 * unchanged at the first step of the pass and refuses a bad one with 400
 * `invalid swarm start: ...` or 422 `invalid swarm goal: <reason>`.
 * POST /api/swarms/{id}/stop takes no body. Both answer the execution with
 * the snapshot identity, the source, the goal of the current pass and the
 * last step; a refusal (400, 403, 404, 409, 422) carries the server's reason.
 * The panel does not judge the commit: the server names what is wrong with it.
 */

import { request } from './http';
import type { SnapshotIdentity } from './pipelines';

export interface SwarmExecutionResult {
  swarmId: string;
  execution: string;
  definition?: SnapshotIdentity | null;
  source?: { origin: string; commit: string } | null;
  /** Goal of the current pass; absent before the first start. */
  goal?: string;
  step?: { sequence: number; nodeId: string; state: string } | null;
}

/** What a start names: the definition, the goal of the pass and, on the
 * first start only, the commit as typed. */
export interface SwarmStart {
  definition: string;
  goal: string;
  commit?: string;
}

/** Body of a start: the goal goes exactly as typed, the server keeps it so;
 * the commit goes only when one is typed. */
export function startBody({ definition, goal, commit = '' }: SwarmStart): SwarmStart {
  const typed = commit.trim();
  return typed ? { definition, goal, commit: typed } : { definition, goal };
}

const swarmPath = (id: string) => `/api/swarms/${encodeURIComponent(id)}`;

export const executionApi = {
  start: (swarmId: string, start: SwarmStart) =>
    request<SwarmExecutionResult>('POST', `${swarmPath(swarmId)}/start`, startBody(start)),
  stop: (swarmId: string) => request<SwarmExecutionResult>('POST', `${swarmPath(swarmId)}/stop`),
};
