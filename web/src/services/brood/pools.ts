/**
 * brood: Proxmox connections and their pools (administrator only).
 * Shapes follow go/brood/server/proxmox.go, proxmox_connections.go and
 * pools.go. The token secret is write-only.
 */
import { request } from './http';

export interface PoolUsage {
  state: string;
  maxActors: string;
  usedActors: string;
  cpuMillicores: string;
  usedCPUMillicores: string;
  memoryBytes: string;
  usedMemoryBytes: string;
  diskBytes: string;
  usedDiskBytes: string;
}

export interface Pool {
  name: string;
  endpoint: string;
  tokenID: string;
  node: string;
  resourcePool: string;
  storage: string;
  network: string;
  /** The connection can be activated: template and capacity are installed. */
  runtimeAvailable: boolean;
  /** Present once the pool is activated. */
  managedPool?: PoolUsage;
  /** Present for connections added in the panel; absent for ones from .env. */
  uid?: string;
}

export interface NewConnection {
  name: string;
  endpoint: string;
  tokenID: string;
  tokenSecret: string;
  caPEM: string;
  node: string;
  resourcePool: string;
  storage: string;
  network: string;
  template: string;
  templateDigest: string;
  capacity: { actors: number; cpuMillicores: string; memoryBytes: string; diskGiB: number };
}

export type PoolAction = 'activate' | 'drain' | 'enable';

const poolPath = (name: string) => `/api/proxmox-pools/${encodeURIComponent(name)}`;

export const poolsApi = {
  /** Pools with usage, joined with the uid of their panel-managed connection. */
  list: async (): Promise<Pool[]> => {
    const [pools, connections] = await Promise.all([
      request<{ items: Pool[] }>('GET', '/api/proxmox-pools'),
      request<{ items: { uid: string; name: string }[] }>('GET', '/api/proxmox-connections'),
    ]);
    const uids = new Map(connections.items.map((item) => [item.name, item.uid]));
    return pools.items.map((pool) => ({ ...pool, uid: uids.get(pool.name) }));
  },
  createConnection: (body: NewConnection) =>
    request<unknown>('POST', '/api/proxmox-connections', body),
  deleteConnection: (uid: string) =>
    request<void>('DELETE', `/api/proxmox-connections/${encodeURIComponent(uid)}`),
  act: (name: string, action: PoolAction) =>
    request<unknown>('POST', `${poolPath(name)}/${action}`),
  deletePool: (name: string) => request<void>('DELETE', poolPath(name)),
};
