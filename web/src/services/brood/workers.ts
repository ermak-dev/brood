/**
 * brood: workers of a swarm. The API still calls a worker an "actor"
 * (go/brood/server/api_actors.go, managed_json.go, managed_runtime.go).
 */
import { request } from './http';

export type WorkerRole = 'worker' | 'master';
export type WorkerAction = 'resume' | 'stop' | 'reset';

export interface Worker {
  name: string;
  role: string;
  /** provisioning, running, stopping, stopped, error or deleting. */
  phase: string;
  error: string;
  pool: string;
  node: string;
  ctid: number | null;
  origin: string;
}

export interface NewWorker {
  name: string;
  profile: string;
  role: WorkerRole;
  origin: string;
}

interface WireWorker {
  metadata?: { name?: string };
  role?: string;
  phase?: string;
  status?: { errorMessage?: string };
  placement?: { node?: string; ctid?: number; pool?: string };
  origin?: string;
}

const fromWire = (wire: WireWorker): Worker => ({
  name: wire.metadata?.name ?? '',
  role: wire.role ?? '',
  phase: wire.phase ?? '',
  error: wire.status?.errorMessage ?? '',
  pool: wire.placement?.pool ?? '',
  node: wire.placement?.node ?? '',
  ctid: wire.placement?.ctid || null,
  origin: wire.origin ?? '',
});

const segment = encodeURIComponent;
const swarmPath = (swarm: string) => `/api/swarms/${segment(swarm)}`;
const workerPath = (swarm: string, name: string) => `${swarmPath(swarm)}/actors/${segment(name)}`;

export const workersApi = {
  list: async (swarm: string) =>
    (await request<{ results: WireWorker[] }>('GET', `${swarmPath(swarm)}/actors`)).results.map(
      fromWire
    ),
  /** Names of the pool profiles a worker can be created from. */
  profiles: async (swarm: string) =>
    (
      await request<{ results: { metadata: { name: string } }[] }>(
        'GET',
        `${swarmPath(swarm)}/templates`
      )
    ).results.map((profile) => profile.metadata.name),
  /** The server rejects any field beyond these four. */
  create: (swarm: string, { name, profile, role, origin }: NewWorker) =>
    request<unknown>('POST', `${swarmPath(swarm)}/actors`, {
      metadata: { name },
      actorTemplate: { name: profile },
      role,
      ...(origin ? { origin } : {}),
    }),
  act: (swarm: string, name: string, action: WorkerAction) =>
    request<unknown>('POST', workerPath(swarm, name), { action }),
  remove: (swarm: string, name: string) => request<void>('DELETE', workerPath(swarm, name)),
  /** A GET here issues a short-lived actor view and redirects to the terminal. */
  terminal: (swarm: string, name: string) => `${swarmPath(swarm)}/proxy/actors/${segment(name)}/`,
};
