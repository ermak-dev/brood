/**
 * brood: Substrate entities through the brood server (/api, Django -> ateapi).
 *
 * Objects travel in ateapi's canonical protojson -- the shape `kubectl ate get
 * -o yaml` prints and Substrate's demo templates use: camelCase fields, int64
 * as strings, enums by name. Editors change the fields they know on the object
 * as read and send the rest back untouched, so nothing a form does not show is
 * lost.
 */

import { request } from './http';

export interface Meta {
  name: string;
  atespace?: string;
  uid?: string;
  version?: string;
  createTime?: string;
  updateTime?: string;
}

export interface ObjectRef {
  atespace?: string;
  name: string;
}

export interface Selector {
  matchLabels?: Record<string, string>;
}

export interface Atespace {
  metadata: Meta;
}

export interface ActorStatus {
  state?: string;
  assignedNode?: string;
  workerAssignment?: { worker?: ObjectRef; workerPod?: string; nodeName?: string };
  crash?: Record<string, unknown>;
  snapshots?: unknown[];
}

export interface Actor {
  metadata: Meta;
  actorTemplate?: ObjectRef;
  workerSelector?: Selector;
  sourceTag?: ObjectRef;
  status?: ActorStatus;
}

export interface EnvVar {
  name: string;
  value?: string;
}

export interface VolumeMount {
  name: string;
  mountPath: string;
}

export interface Container {
  name: string;
  image: string;
  command?: string[];
  args?: string[];
  env?: EnvVar[];
  volumeMounts?: VolumeMount[];
  [field: string]: unknown;
}

export interface Volume {
  name: string;
  [source: string]: unknown;
}

export interface Limits {
  cpuMillicores?: string;
  memoryBytes?: string;
}

export interface ActorTemplate {
  metadata: Meta;
  workerSelector?: Selector;
  containers: Container[];
  volumes?: Volume[];
  resources?: { limits?: Limits };
  snapshotConfig?: {
    preferredFidelity?: string;
    storageLocation?: string;
    goldenSnapshotConfig?: { mode?: string };
    [field: string]: unknown;
  };
  /** Output only: the golden snapshot ateapi takes from the template's golden
   * actor (named by the template's uid). */
  status?: {
    goldenSnapshotStatus?: { goldenTag?: ObjectRef; snapshotTime?: string; errorMessage?: string };
  };
  [field: string]: unknown;
}

export interface WorkerResources {
  cpuMillicores?: string;
  memoryBytes?: string;
  actors?: number;
}

export interface Worker {
  metadata: Meta;
  workerNamespace?: string;
  workerPool?: string;
  workerPod?: string;
  nodeName?: string;
  workerSpiffeId?: string;
  labels?: Record<string, string>;
  status?: {
    state?: string;
    capacity?: WorkerResources;
    allocated?: WorkerResources;
    ips?: string[];
    sandboxClasses?: unknown[];
  };
}

export interface WorkerPool {
  metadata: {
    name: string;
    uid?: string;
    resourceVersion?: string;
    creationTimestamp?: string;
    [field: string]: unknown;
  };
  spec: {
    replicas: number;
    workerImage: string;
    sandboxClasses: { name: string; [field: string]: unknown }[];
    template?: {
      resources?: {
        requests?: Record<string, string>;
        limits?: Record<string, string>;
        [field: string]: unknown;
      };
      [field: string]: unknown;
    };
    [field: string]: unknown;
  };
  status?: {
    replicas?: number;
    readyReplicas?: number;
    sandboxClasses?: {
      name: string;
      configRef?: { name: string };
      pendingConfigRef?: { name: string };
      conditions?: { type: string; status: string; reason?: string; message?: string }[];
    }[];
  };
  [field: string]: unknown;
}

export interface WorkerPoolDefaults {
  workerImage: string;
  workerImages?: Record<string, string>;
}

export interface Binding {
  role: string;
  members?: string[];
}

export interface AccessPolicy {
  metadata?: Meta;
  bindings?: Binding[];
}

export type ActorAction = 'suspend' | 'resume';

const path = encodeURIComponent;
const projectPath = (project: string) => `/api/swarms/${path(project)}`;
/** '' -- the whole install, otherwise the project's own policy. */
const policyPath = (project: string) =>
  project ? `/api/swarms/${path(project)}/policy` : '/api/policy/global';

async function list<T>(url: string): Promise<T[]> {
  const body = await request<{ results?: T[] }>('GET', url);
  return body?.results ?? [];
}

export interface SwarmScope extends Atespace {
  label: string;
}

export const scopesApi = {
  list: async (): Promise<SwarmScope[]> => {
    const projects = await request<Array<{ id: string; name: string }>>('GET', '/api/projects');
    const groups = await Promise.all(
      projects.map(async (project) => {
        const swarms = await request<
          Array<{
            id: string;
            name: string;
            created_at: string;
          }>
        >('GET', `/api/projects/${path(project.id)}/swarms`);
        return swarms.map((swarm) => ({
          metadata: { name: swarm.id, createTime: swarm.created_at },
          label: `${project.name} / ${swarm.name}`,
        }));
      })
    );
    return groups.flat();
  },
};

export const actorsApi = {
  terminal: (swarm: string, name: string) => `${projectPath(swarm)}/proxy/actors/${path(name)}/`,
  list: (project: string) => list<Actor>(`${projectPath(project)}/actors`),
  get: (project: string, name: string) =>
    request<Actor>('GET', `${projectPath(project)}/actors/${path(name)}`),
  create: (project: string, actor: Actor) =>
    request<Actor>('POST', `${projectPath(project)}/actors`, actor),
  update: (project: string, actor: Actor) =>
    request<Actor>('PUT', `${projectPath(project)}/actors/${path(actor.metadata.name)}`, actor),
  act: (project: string, name: string, action: ActorAction) =>
    request<Actor>('POST', `${projectPath(project)}/actors/${path(name)}`, { action }),
  remove: (project: string, name: string) =>
    request<null>('DELETE', `${projectPath(project)}/actors/${path(name)}`),
};

export const templatesApi = {
  list: (project: string) => list<ActorTemplate>(`${projectPath(project)}/templates`),
  create: (project: string, template: ActorTemplate) =>
    request<ActorTemplate>('POST', `${projectPath(project)}/templates`, template),
  remove: (project: string, name: string) =>
    request<null>('DELETE', `${projectPath(project)}/templates/${path(name)}`),
};

export const workersApi = {
  list: () => list<Worker>('/api/workers'),
  drain: (name: string) =>
    request<Worker>('POST', `/api/workers/${path(name)}`, { action: 'drain' }),
};

// WorkerPool — Kubernetes-объект, не protojson ateapi. Предусловия исходного
// чтения сохраняются: конфликт требует перечитать объект и повторить правку.
export const workerPoolsApi = {
  list: async (): Promise<WorkerPool[]> => {
    const body = await request<{ items: WorkerPool[] | null }>('GET', '/api/worker-pools');
    if (!body || !('items' in body) || (body.items !== null && !Array.isArray(body.items))) {
      throw new Error('Kubernetes returned an invalid WorkerPool list');
    }
    return body.items ?? [];
  },
  defaults: () => request<WorkerPoolDefaults>('GET', '/api/worker-pool-defaults'),
  get: (name: string) => request<WorkerPool>('GET', `/api/worker-pools/${path(name)}`),
  create: (pool: WorkerPool) => request<WorkerPool>('POST', '/api/worker-pools', pool),
  update: (pool: WorkerPool) =>
    request<WorkerPool>('PUT', `/api/worker-pools/${path(pool.metadata.name)}`, pool),
  remove: (pool: WorkerPool) =>
    request<null>('DELETE', `/api/worker-pools/${path(pool.metadata.name)}`, {
      metadata: { uid: pool.metadata.uid, resourceVersion: pool.metadata.resourceVersion },
    }),
};

export const policyApi = {
  get: (project: string) => request<AccessPolicy>('GET', policyPath(project)),
  create: (project: string, policy: AccessPolicy) =>
    request<AccessPolicy>('POST', policyPath(project), policy),
  update: (project: string, policy: AccessPolicy) =>
    request<AccessPolicy>('PUT', policyPath(project), policy),
  remove: (project: string) => request<null>('DELETE', policyPath(project)),
};
