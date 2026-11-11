import { request } from './http';

export interface AccessProject {
  id: string;
  name: string;
  /** Число роёв проекта в списке роёв вызывающего (#234): его несёт только
   * строка `GET /api/projects`, у созданного проекта поля нет. */
  swarmCount?: number;
}

export type SwarmRole = 'owner' | 'editor' | 'viewer';

export interface ShardCapacity {
  worker_selector: Record<string, string>;
  max_swarms: string | null;
  max_actors: string | null;
  max_cpu_millicores: string | null;
  max_memory_bytes: string | null;
}

export interface ShardUsage {
  swarms: string;
  actors: string;
  cpu_millicores: string;
  memory_bytes: string;
}

export interface ProjectDetail extends AccessProject {
  capacity: ShardCapacity;
  usage: ShardUsage | null;
  usage_available: boolean;
  usage_error?: string;
}

export interface SwarmMember {
  user_id: string;
  username: string;
  display_name: string;
  role: SwarmRole;
  disabled: boolean;
}

export interface AccessSwarm {
  id: string;
  project_id: string;
  owner_id: string;
  owner_username: string;
  role: SwarmRole;
  retry?: 'create' | 'delete' | '';
  name: string;
  atespace_name: string;
  state: string;
  created_at: string;
  updated_at: string;
  /** Сводка исполнения в строке списка (#234): её несёт только строка
   * `GET /api/projects/{id}/swarms`, у одного роя и созданного полей нет. */
  execution?: string;
  openRequests?: number;
}

export interface AccessUser {
  username: string;
  display_name: string;
  is_admin: boolean;
  disabled: boolean;
}

export interface CreateUser {
  username: string;
  password: string;
  display_name: string;
  is_admin: boolean;
  disabled?: boolean;
}

/** Git remote установки (BROOD_GIT_REMOTES). Токен сервер не отдаёт; панель
 * и не читает ничего, кроме origin и признака настроенных учётных данных. */
export interface GitRemote {
  origin: string;
  credentialConfigured: boolean;
}

const segment = encodeURIComponent;
const projectPath = (id: string) => `/api/projects/${segment(id)}`;

async function readRepository(id: string): Promise<string> {
  const body = await request<{ origin?: unknown } | null>('GET', `${projectPath(id)}/repository`);
  if (typeof body?.origin !== 'string') {
    throw new Error('Server returned an invalid project repository');
  }
  return body.origin;
}

async function readGitRemotes(): Promise<GitRemote[]> {
  const body = await request<{ items?: unknown } | null>('GET', '/api/git-remotes');
  const items = Array.isArray(body?.items) ? body.items : null;
  const remotes = items?.map((item: unknown) => {
    const row = item as { origin?: unknown; credentialConfigured?: unknown } | null;
    return typeof row?.origin === 'string' && typeof row.credentialConfigured === 'boolean'
      ? { origin: row.origin, credentialConfigured: row.credentialConfigured }
      : null;
  });
  if (!remotes || remotes.some((remote) => remote === null)) {
    throw new Error('Server returned an invalid Git remote list');
  }
  return remotes as GitRemote[];
}
const swarmPath = (id: string) => `/api/swarms/${segment(id)}`;
const userPath = (username: string) => `/api/admin/users/${segment(username)}`;

// Повтор создания с тем же именем и повтор удаления возобновляют операцию.
// Отдельного маршрута retry нет: сервер восстанавливает исходную операцию.
export const accessApi = {
  users: () => request<AccessUser[]>('GET', '/api/admin/users'),
  user: (username: string) => request<AccessUser>('GET', userPath(username)),
  updateUser: (username: string, properties: Omit<AccessUser, 'username'>) =>
    request<void>('PUT', userPath(username), properties),
  deleteUser: (username: string) => request<void>('DELETE', userPath(username)),
  createUser: (body: CreateUser) =>
    request<Omit<AccessUser, 'disabled'>>('POST', '/api/admin/users', body),
  setDisabled: (username: string, disabled: boolean) =>
    request<void>('POST', `${userPath(username)}/${disabled ? 'disable' : 'enable'}`),
  setRole: (username: string, isAdmin: boolean) =>
    request<void>('POST', `${userPath(username)}/role`, { is_admin: isAdmin }),
  members: (projectId: string) => request<AccessUser[]>('GET', `${projectPath(projectId)}/members`),
  resetPassword: (username: string, password: string) =>
    request<void>('POST', `${userPath(username)}/password-reset`, { new_password: password }),
  projects: () => request<AccessProject[]>('GET', '/api/projects'),
  project: (id: string) => request<ProjectDetail>('GET', projectPath(id)),
  setCapacity: (id: string, capacity: ShardCapacity) =>
    request<void>('PUT', `${projectPath(id)}/capacity`, capacity),
  /** Repository проекта: '' — не выбран. Читают участники проекта. */
  repository: readRepository,
  /** Только администратор: '' очищает выбор. */
  setRepository: (id: string, origin: string) =>
    request<void>('PUT', `${projectPath(id)}/repository`, { origin }),
  /** Только администратор: remotes, из которых выбирается repository. */
  gitRemotes: readGitRemotes,
  createProject: (name: string) => request<AccessProject>('POST', '/api/projects', { name }),
  deleteProject: (id: string) => request<void>('DELETE', projectPath(id)),
  addMember: (projectId: string, username: string) =>
    request<void>('POST', `${projectPath(projectId)}/members`, { username }),
  removeMember: (projectId: string, username: string) =>
    request<void>('DELETE', `${projectPath(projectId)}/members/${segment(username)}`),
  swarms: (projectId: string) => request<AccessSwarm[]>('GET', `${projectPath(projectId)}/swarms`),
  /** Один рой по ID; чужой рой сервер не отличает от отсутствующего (404). */
  swarm: (id: string) => request<AccessSwarm>('GET', swarmPath(id)),
  swarmMembers: (id: string) => request<SwarmMember[]>('GET', `${swarmPath(id)}/members`),
  setSwarmMember: (id: string, username: string, role: SwarmRole) =>
    request<void>('POST', `${swarmPath(id)}/members`, { username, role }),
  removeSwarmMember: (id: string, username: string) =>
    request<void>('DELETE', `${swarmPath(id)}/members/${segment(username)}`),
  createSwarm: (projectId: string, name: string) =>
    request<AccessSwarm>('POST', `${projectPath(projectId)}/swarms`, { name }),
  deleteSwarm: (id: string) => request<void>('DELETE', swarmPath(id)),
  transferSwarm: (id: string, username: string) =>
    request<void>('POST', `${swarmPath(id)}/transfer`, { username }),
};
