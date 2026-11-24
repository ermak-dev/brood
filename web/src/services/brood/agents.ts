/**
 * brood: agents of the swarms the caller may see. The API still calls an
 * agent an "actor" (go/brood/server/
 * api_lists.go, api_actors.go, managed_json.go, managed_runtime.go).
 */
import { request } from './http';

export type AgentAction = 'resume' | 'stop' | 'reset';

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
  /** The pool profile the agent was created from. */
  profile: string;
  /** The repository cloned into the workspace; '' when none. */
  origin: string;
  /** What the pool gives the agent; decimal strings, '' when unknown. */
  cpuMillicores: string;
  memoryBytes: string;
  diskBytes: string;
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
  profile: string;
  roleId: string;
  origin: string;
}

interface WireAgent {
  metadata?: { name?: string; swarm?: string };
  swarmName?: string;
  projectID?: string;
  projectName?: string;
  role?: string;
  phase?: string;
  status?: { errorMessage?: string; errorCode?: string };
  placement?: { ctid?: number; pool?: string; node?: string };
  profile?: string;
  origin?: string;
  templateDefinition?: {
    resources?: { cpuMillicores?: string; memoryBytes?: string; diskBytes?: string };
  };
  createdAt?: string;
  work?: {
    activity?: string;
    detail?: string;
    branch?: string;
    uncommitted?: number;
    unpushed?: number;
    model?: string;
  };
}

const fromWire = (wire: WireAgent): Agent => ({
  name: wire.metadata?.name ?? '',
  swarmId: wire.metadata?.swarm ?? '',
  swarmName: wire.swarmName ?? '',
  projectId: wire.projectID ?? '',
  projectName: wire.projectName ?? '',
  role: wire.role ?? '',
  phase: wire.phase ?? '',
  error: wire.status?.errorMessage ?? '',
  errorCode: wire.status?.errorCode ?? '',
  pool: wire.placement?.pool ?? '',
  node: wire.placement?.node ?? '',
  ctid: wire.placement?.ctid || null,
  profile: wire.profile ?? '',
  origin: wire.origin ?? '',
  cpuMillicores: wire.templateDefinition?.resources?.cpuMillicores ?? '',
  memoryBytes: wire.templateDefinition?.resources?.memoryBytes ?? '',
  diskBytes: wire.templateDefinition?.resources?.diskBytes ?? '',
  createdAt: wire.createdAt ?? '',
  activity: wire.work?.activity ?? '',
  activityDetail: wire.work?.detail ?? '',
  branch: wire.work?.branch ?? '',
  uncommitted: wire.work?.uncommitted ?? null,
  unpushed: wire.work?.unpushed ?? null,
  model: wire.work?.model ?? '',
});

const segment = encodeURIComponent;
const swarmPath = (swarm: string) => `/api/swarms/${segment(swarm)}`;
const agentPath = (swarm: string, name: string) => `${swarmPath(swarm)}/actors/${segment(name)}`;

export const agentsApi = {
  list: async () =>
    (await request<{ results: WireAgent[] }>('GET', '/api/agents')).results.map(fromWire),
  /** Names of the pool profiles an agent of the swarm can be created from. */
  profiles: async (swarm: string) =>
    (
      await request<{ results: { metadata: { name: string } }[] }>(
        'GET',
        `${swarmPath(swarm)}/templates`
      )
    ).results.map((profile) => profile.metadata.name),
  /** The server rejects any field beyond these four. */
  create: (swarm: string, { name, profile, roleId, origin }: NewAgent) =>
    request<unknown>('POST', `${swarmPath(swarm)}/actors`, {
      metadata: { name },
      actorTemplate: { name: profile },
      roleId,
      ...(origin ? { origin } : {}),
    }),
  act: (swarm: string, name: string, action: AgentAction) =>
    request<unknown>('POST', agentPath(swarm, name), { action }),
  remove: (swarm: string, name: string) => request<void>('DELETE', agentPath(swarm, name)),
  /** A GET here issues a short-lived actor view and redirects to the terminal. */
  terminal: (swarm: string, name: string) =>
    `${swarmPath(swarm)}/proxy/actors/${segment(name)}/?mode=rw`,
  /** The same session as a read-only snapshot: nothing typed reaches the agent. */
  view: (swarm: string, name: string) =>
    `${swarmPath(swarm)}/proxy/actors/${segment(name)}/?mode=ro`,
};
