/**
 * brood: pipeline runs and their human gates (#169, engine #168).
 *
 * Pipeline run API contract.
 * GET /api/swarms/{swarm}/pipeline-runs lists runs visible in that swarm;
 * GET /api/pipeline-runs/{run} returns the run and its current gate, and
 * GET /api/pipeline-runs/{run}/graph returns {svg} with state styling embedded.
 * POST /api/pipeline-runs/{run}/nodes/{node}/answer accepts {answer, visitSequence}.
 * The server must authorize every operation against the run's swarm, return
 * a reason on refusal, and atomically reject a stale or already answered visit.
 * Gate metadata and a positive, safe-integer visitSequence come from the server.
 */

import { request } from './http';

export type RunStatus = 'running' | 'waiting_human' | 'succeeded' | 'failed';

export type NodeStatus = 'pending' | 'active' | 'done' | 'failed' | 'skipped' | 'unconfirmed';

export type NodeKind = 'actor' | 'human' | 'condition' | 'land' | 'exit' | 'start';

export interface RunNode {
  id: string;
  kind: NodeKind;
  status: NodeStatus;
  label: string;
  /** What the node concluded: success text, refusal reason or verdict. */
  outcome?: string;
}

export interface PipelineRun {
  id: string;
  name: string;
  status: RunStatus;
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

export interface RunGraph {
  svg: string;
}

const segment = encodeURIComponent;
const runPath = (id: string) => `/api/pipeline-runs/${segment(id)}`;

export const pipelinesApi = {
  listRuns: (swarmId: string) =>
    request<PipelineRun[]>('GET', `/api/swarms/${segment(swarmId)}/pipeline-runs`),
  getRun: (id: string) => request<PipelineRun>('GET', runPath(id)),
  answerGate: (runId: string, nodeId: string, answer: string, visitSequence: number) =>
    request<PipelineRun>('POST', `${runPath(runId)}/nodes/${segment(nodeId)}/answer`, {
      answer,
      visitSequence,
    }),
  getRunGraph: (id: string) => request<RunGraph>('GET', `${runPath(id)}/graph`),
};
