/**
 * brood: start and stop of a swarm's graph (#211, page #218).
 *
 * POST /api/swarms/{id}/start takes {definition, commit}: the first start
 * names a definition of the catalog of the swarm's project and the full
 * commit of the project repository to pin; a restart names the pinned
 * definition and may leave the commit out. POST /api/swarms/{id}/stop takes
 * no body. Both answer the execution with the snapshot identity, the source
 * and the last step; a refusal (403, 404, 409, 422) carries the server's
 * reason. The panel does not judge the commit: the server names what is
 * wrong with it (until #228 resolves a branch, the first start needs it).
 */

import { request } from './http';
import type { SnapshotIdentity } from './pipelines';

export interface SwarmExecutionResult {
  swarmId: string;
  execution: string;
  definition?: SnapshotIdentity | null;
  source?: { origin: string; commit: string } | null;
  step?: { sequence: number; nodeId: string; state: string } | null;
}

export interface SwarmStartBody {
  definition: string;
  commit?: string;
}

/** Body of a start: the commit goes only when one is typed. */
export function startBody(definition: string, commit: string): SwarmStartBody {
  const typed = commit.trim();
  return typed ? { definition, commit: typed } : { definition };
}

const swarmPath = (id: string) => `/api/swarms/${encodeURIComponent(id)}`;

export const executionApi = {
  start: (swarmId: string, definition: string, commit = '') =>
    request<SwarmExecutionResult>(
      'POST',
      `${swarmPath(swarmId)}/start`,
      startBody(definition, commit)
    ),
  stop: (swarmId: string) => request<SwarmExecutionResult>('POST', `${swarmPath(swarmId)}/stop`),
};
