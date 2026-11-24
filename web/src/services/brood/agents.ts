/**
 * brood: agents of the swarms the caller may see
 * (go/brood/server/api_actors.go, api_lists.go, managed_json.go).
 */
import { request } from './http';

export type AgentAction = 'start' | 'stop' | 'reset';

export interface Agent {
  name: string;
  swarmId: string;
  swarmName: string;
  projectId: string;
  projectName: string;
  role: string;
  /** provisioning, running, stopping, stopped, error or deleting. */
  phase: string;
  /** The server's diagnostic in English and its stable code; both empty when fine. */
  error: string;
  errorCode: string;
  pool: string;
  /** The Proxmox node of the pool. */
  node: string;
  ctid: number | null;
  /** The template profile of the container. */
  profile: string;
  /** The repository cloned into the workspace; '' when none. */
  origin: string;
  /** What the pool gives the agent; 0 when unknown. */
  cpuMillicores: number;
  memoryBytes: number;
  diskBytes: number;
  /** RFC 3339. */
  createdAt: string;
  /** The verdict on a running agent: free, busy, idle, needs_action, waiting,
   * error, offline or unknown; '' for an agent that is not running. Only
   * `free` means the agent can take new work. */
  activity: string;
  /** The server's explanation of the verdict in English; '' when none. */
  activityDetail: string;
  /** The branch of the clone; '' when unknown or there is no clone. */
  branch: string;
  /** Changed and untracked paths, and commits no remote branch has; null
   * when the clone was not measured. */
  uncommitted: number | null;
  unpushed: number | null;
  /** The model of the session; '' when unknown. */
  model: string;
}

export interface NewAgent {
  name: string;
  /** A pool from `profiles`. */
  pool: string;
  roleId: string;
  origin: string;
}

interface WireAgent {
  name: string;
  swarm: string;
  swarmName: string;
  projectId: string;
  projectName: string;
  role: string;
  phase: string;
  error: string;
  errorCode: string;
  pool: string;
  node: string;
  ctid: number;
  profile: string;
  origin: string;
  cpuMillicores: number;
  memoryBytes: number;
  diskBytes: number;
  createdAt: string;
  work?: {
    activity: string;
    detail?: string;
    branch?: string;
    uncommitted?: number;
    unpushed?: number;
    model?: string;
  };
}

const fromWire = ({ swarm, ctid, work, ...wire }: WireAgent): Agent => ({
  ...wire,
  swarmId: swarm,
  ctid: ctid || null,
  activity: work?.activity ?? '',
  activityDetail: work?.detail ?? '',
  branch: work?.branch ?? '',
  uncommitted: work?.uncommitted ?? null,
  unpushed: work?.unpushed ?? null,
  model: work?.model ?? '',
});

const segment = encodeURIComponent;
const swarmPath = (swarm: string) => `/api/swarms/${segment(swarm)}`;
const agentPath = (swarm: string, name: string) => `${swarmPath(swarm)}/agents/${segment(name)}`;

export const agentsApi = {
  list: async () => (await request<WireAgent[]>('GET', '/api/agents')).map(fromWire),
  /** Names of the pools an agent of the swarm can be created in. */
  profiles: (swarm: string) => request<string[]>('GET', `${swarmPath(swarm)}/profiles`),
  /** The server rejects any field beyond these four. */
  create: (swarm: string, { name, pool, roleId, origin }: NewAgent) =>
    request<unknown>('POST', `${swarmPath(swarm)}/agents`, {
      name,
      pool,
      roleId,
      ...(origin ? { origin } : {}),
    }),
  act: (swarm: string, name: string, action: AgentAction) =>
    request<unknown>('POST', agentPath(swarm, name), { action }),
  remove: (swarm: string, name: string) => request<void>('DELETE', agentPath(swarm, name)),
  /** A GET here issues a short-lived terminal ticket and redirects to the terminal. */
  terminal: (swarm: string, name: string) =>
    `${swarmPath(swarm)}/proxy/agents/${segment(name)}/?mode=rw`,
  /** The same session as a read-only snapshot: nothing typed reaches the agent. */
  view: (swarm: string, name: string) =>
    `${swarmPath(swarm)}/proxy/agents/${segment(name)}/?mode=ro`,
};
