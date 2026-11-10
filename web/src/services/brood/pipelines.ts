/**
 * brood: the execution and graph of a swarm (#169, engine #168, swarm #206)
 * and the project workflow catalog.
 *
 * Pipeline run API contract, as the swarm page reads it.
 * GET /api/pipeline-runs/{run} returns the swarm execution and
 * GET /api/pipeline-runs/{run}/graph returns {svg} with state styling embedded,
 * and may list the graph nodes with the instruction each one carries.
 * Since #206 the graph and its execution belong to the swarm: {run} is the
 * swarm ID, and a visible swarm without a graph answers 404 (getSwarmRun).
 * The run carries `definition`, the identity of the snapshot its first start
 * pinned (#218), not the current catalog definition; an older server omits it.
 * GET /api/projects/{project}/workflow-catalog lists published definitions and
 * GET /api/projects/{project}/workflows/{name}/graph?version=&revision= draws
 * one of them for the first start of a swarm (#218).
 * Operator questions are answered through the swarm request API (#216,
 * requests.ts), not through the run. The server must authorize every
 * operation against the run's swarm and return a reason on refusal.
 */

import { BroodApiError, request } from './http';

/** The swarm execution on the run wire: the server names `waiting` as
 * `waiting_human` and passes `idle` and `stopped` through (#206). */
export type RunStatus = 'idle' | 'running' | 'waiting_human' | 'succeeded' | 'failed' | 'stopped';

export type NodeStatus = 'pending' | 'active' | 'done' | 'failed' | 'skipped' | 'unconfirmed';

export type NodeKind = 'actor' | 'human' | 'condition' | 'land' | 'exit' | 'start';

/** One step: a visit of a graph node, in sequence order. */
export interface RunNode {
  id: string;
  kind: NodeKind;
  status: NodeStatus;
  label: string;
  /** What the node concluded: success text, refusal reason or verdict. */
  outcome?: string;
  /** Name of the actor that served the step, when the server knows it. */
  actorName?: string;
  /** Step number and recorded moments; absent when the server sends none. */
  sequence?: number;
  startedAt?: string;
  finishedAt?: string;
}

/** Identity of a definition: in the catalog, or as a swarm pinned it. */
export interface SnapshotIdentity {
  name: string;
  version: number;
  revision: string;
}

export interface PipelineRun {
  id: string;
  /** Name of the graph definition, not of the swarm. */
  name: string;
  /** The snapshot the first start pinned; a newer catalog version leaves it. */
  definition?: SnapshotIdentity;
  status: RunStatus;
  swarmId?: string;
  swarmName?: string;
  projectId?: string;
  projectName?: string;
  /** Username of the swarm owner. */
  owner?: string;
  /** Null when the run has no recorded start time. */
  startedAt: string | null;
  finishedAt?: string;
  /** Label of the node the run is on now. */
  currentNode?: string;
  nodes: RunNode[];
}

/** A graph node with what the graph asks of it: the actor prompt or the
 * operator question. */
export interface GraphNode {
  id: string;
  kind: NodeKind;
  prompt?: string;
  question?: string;
}

export interface RunGraph {
  svg: string;
  nodes?: GraphNode[];
}

export interface WorkflowDefinition {
  name: string;
  version: number;
  revision: string;
  publishedAt: string | null;
}

const segment = encodeURIComponent;
const projectPath = (id: string) => `/api/projects/${segment(id)}`;
const runPath = (id: string) => `/api/pipeline-runs/${segment(id)}`;

export const pipelinesApi = {
  listDefinitions: async (projectId: string) => {
    const response = await request<{ workflows?: unknown }>(
      'GET',
      `${projectPath(projectId)}/workflow-catalog`
    );
    if (
      !Array.isArray(response.workflows) ||
      !response.workflows.every((item: unknown) => {
        if (!item || typeof item !== 'object') return false;
        const d = item as Record<string, unknown>;
        return (
          typeof d.name === 'string' &&
          d.name.length > 0 &&
          typeof d.version === 'number' &&
          Number.isSafeInteger(d.version) &&
          d.version > 0 &&
          typeof d.revision === 'string' &&
          /^[0-9a-f]{32}$/.test(d.revision) &&
          (d.publishedAt === null || typeof d.publishedAt === 'string')
        );
      })
    )
      throw new Error('Invalid workflow catalog response');
    return response.workflows as WorkflowDefinition[];
  },
  getDefinitionGraph: (projectId: string, name: string, version: number, revision: string) =>
    request<RunGraph>(
      'GET',
      `${projectPath(projectId)}/workflows/${segment(name)}/graph?version=${version}&revision=${segment(revision)}`
    ),
  /** The execution of a swarm the caller can already read; null when the
   * swarm has no graph yet (the run route answers 404 for it). */
  getSwarmRun: async (swarmId: string): Promise<PipelineRun | null> => {
    try {
      return await request<PipelineRun>('GET', runPath(swarmId));
    } catch (error) {
      if (error instanceof BroodApiError && error.status === 404) return null;
      throw error;
    }
  },
  getRunGraph: (id: string) => request<RunGraph>('GET', `${runPath(id)}/graph`),
};
