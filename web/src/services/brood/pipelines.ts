/**
 * brood: pipeline runs and their human gates (#169, engine #168).
 *
 * Pipeline run API contract.
 * GET /api/swarms/{swarm}/pipeline-runs lists runs visible in that swarm;
 * GET /api/pipeline-runs/{run} returns the run and its current gate, and
 * GET /api/pipeline-runs/{run}/graph returns {svg} with state styling embedded,
 * and may list the graph nodes with the instruction each one carries.
 * POST /api/pipeline-runs/{run}/nodes/{node}/answer accepts {answer, visitSequence}.
 * Since #206 the graph and its execution belong to the swarm: {run} is the
 * swarm ID, and a visible swarm without a graph answers 404 (getSwarmRun).
 * The server must authorize every operation against the run's swarm, return
 * a reason on refusal, and atomically reject a stale or already answered visit.
 * Gate metadata and a positive, safe-integer visitSequence come from the server.
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

export interface PipelineRun {
  id: string;
  /** Name of the graph definition, not of the swarm. */
  name: string;
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
  /** Present only for the current waiting human gate. */
  gate?: HumanGate;
}

export type GateKind = 'yes_no' | 'multiple_choice' | 'freeform' | 'confirmation';

export interface GateOption {
  id: string;
  label: string;
  accelerator?: string;
}

export interface HumanGate {
  runId: string;
  nodeId: string;
  /** Monotonic visit identity, checked atomically by the server on answer. */
  visitSequence: number;
  question: string;
  kind: GateKind;
  options: GateOption[];
  deadline?: string;
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
  listRuns: (swarmId: string) =>
    request<PipelineRun[]>('GET', `/api/swarms/${segment(swarmId)}/pipeline-runs`),
  getRun: (id: string) => request<PipelineRun>('GET', runPath(id)),
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
  answerGate: (runId: string, nodeId: string, answer: string, visitSequence: number) =>
    request<PipelineRun>('POST', `${runPath(runId)}/nodes/${segment(nodeId)}/answer`, {
      answer,
      visitSequence,
    }),
  getRunGraph: (id: string) => request<RunGraph>('GET', `${runPath(id)}/graph`),
};
