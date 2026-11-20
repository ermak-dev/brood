/**
 * brood: users, projects, members and swarms of the Go server.
 *
 * Shapes follow go/brood/server/api.go, capacity_api.go, members_read.go,
 * git_repository_api.go and go/brood/auth/service.go. The server stays the
 * authorization authority; a refusal carries its reason in `detail`.
 */
import { request } from './http';

export interface User {
  username: string;
  display_name: string;
  is_admin: boolean;
  disabled: boolean;
}

export interface NewUser {
  username: string;
  password: string;
  display_name: string;
  is_admin: boolean;
}

export interface Project {
  id: string;
  name: string;
  /** Swarms of the project the caller sees; only list rows carry it. */
  swarmCount?: number;
}

/** Limits are decimal strings; null means unlimited. */
export interface ProjectLimits {
  max_swarms: string | null;
  max_actors: string | null;
  max_cpu_millicores: string | null;
  max_memory_bytes: string | null;
}

export const LIMIT_FIELDS = [
  'max_swarms',
  'max_actors',
  'max_cpu_millicores',
  'max_memory_bytes',
] as const satisfies ReadonlyArray<keyof ProjectLimits>;

export interface Swarm {
  id: string;
  project_id: string;
  owner_username: string;
  name: string;
  /** Workers that are not deleted; only list rows carry it. */
  managedActorCount?: number;
}

const segment = encodeURIComponent;
const projectPath = (id: string) => `/api/projects/${segment(id)}`;
const swarmPath = (id: string) => `/api/swarms/${segment(id)}`;
const userPath = (username: string) => `/api/admin/users/${segment(username)}`;

export const accessApi = {
  users: () => request<User[]>('GET', '/api/admin/users'),
  createUser: (body: NewUser) => request<unknown>('POST', '/api/admin/users', body),
  deleteUser: (username: string) => request<void>('DELETE', userPath(username)),
  setDisabled: (username: string, disabled: boolean) =>
    request<void>('POST', `${userPath(username)}/${disabled ? 'disable' : 'enable'}`),
  setAdministrator: (username: string, isAdmin: boolean) =>
    request<void>('POST', `${userPath(username)}/role`, { is_admin: isAdmin }),
  resetPassword: (username: string, password: string) =>
    request<void>('POST', `${userPath(username)}/password-reset`, { new_password: password }),

  projects: () => request<Project[]>('GET', '/api/projects'),
  createProject: (name: string) => request<Project>('POST', '/api/projects', { name }),
  deleteProject: (id: string) => request<void>('DELETE', projectPath(id)),
  members: (id: string) => request<User[]>('GET', `${projectPath(id)}/members`),
  addMember: (id: string, username: string) =>
    request<void>('POST', `${projectPath(id)}/members`, { username }),
  removeMember: (id: string, username: string) =>
    request<void>('DELETE', `${projectPath(id)}/members/${segment(username)}`),
  /** The repository declared by the project's Broodfile; '' when none. */
  repository: async (id: string) =>
    (await request<{ origin: string }>('GET', `${projectPath(id)}/repository`)).origin,
  limits: async (id: string) =>
    (await request<{ capacity: ProjectLimits }>('GET', projectPath(id))).capacity,
  setLimits: (id: string, limits: ProjectLimits) =>
    request<void>('PUT', `${projectPath(id)}/capacity`, limits),

  swarms: (projectId: string) => request<Swarm[]>('GET', `${projectPath(projectId)}/swarms`),
  createSwarm: (projectId: string, name: string) =>
    request<Swarm>('POST', `${projectPath(projectId)}/swarms`, { name }),
  /** Someone else's swarm is indistinguishable from a missing one (404). */
  swarm: (id: string) => request<Swarm>('GET', swarmPath(id)),
  deleteSwarm: (id: string) => request<void>('DELETE', swarmPath(id)),
};
