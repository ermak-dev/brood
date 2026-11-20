/**
 * brood: agents of the swarms the caller may see. The API still calls an
 * agent an "actor" and the ordinary role "worker" (go/brood/server/
 * api_lists.go, api_actors.go, managed_json.go, managed_runtime.go).
 */
import { request } from './http';

export type AgentRole = 'worker' | 'master';
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
  error: string;
  pool: string;
  ctid: number | null;
}

export interface NewAgent {
  name: string;
  profile: string;
  role: AgentRole;
  origin: string;
}

interface WireAgent {
  metadata?: { name?: string; swarm?: string };
  swarmName?: string;
  projectID?: string;
  projectName?: string;
  role?: string;
  phase?: string;
  status?: { errorMessage?: string };
  placement?: { ctid?: number; pool?: string };
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
  pool: wire.placement?.pool ?? '',
  ctid: wire.placement?.ctid || null,
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
  create: (swarm: string, { name, profile, role, origin }: NewAgent) =>
    request<unknown>('POST', `${swarmPath(swarm)}/actors`, {
      metadata: { name },
      actorTemplate: { name: profile },
      role,
      ...(origin ? { origin } : {}),
    }),
  act: (swarm: string, name: string, action: AgentAction) =>
    request<unknown>('POST', agentPath(swarm, name), { action }),
  remove: (swarm: string, name: string) => request<void>('DELETE', agentPath(swarm, name)),
  /** A GET here issues a short-lived actor view and redirects to the terminal. */
  terminal: (swarm: string, name: string) => `${swarmPath(swarm)}/proxy/actors/${segment(name)}/`,
};
