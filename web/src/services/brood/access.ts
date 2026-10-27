import { request } from './http';

export interface AccessProject {
  id: string;
  name: string;
}

export interface AccessSwarm {
  id: string;
  project_id: string;
  owner_id: string;
  name: string;
  atespace_name: string;
  state: string;
  created_at: string;
  updated_at: string;
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
}

const segment = encodeURIComponent;
const projectPath = (id: string) => `/api/projects/${segment(id)}`;
const swarmPath = (id: string) => `/api/swarms/${segment(id)}`;
const userPath = (username: string) => `/api/admin/users/${segment(username)}`;

// Повтор создания с тем же именем и повтор удаления возобновляют операцию.
// Отдельного маршрута retry нет: сервер восстанавливает исходную операцию.
export const accessApi = {
  users: () => request<AccessUser[]>('GET', '/api/admin/users'),
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
  createProject: (name: string) => request<AccessProject>('POST', '/api/projects', { name }),
  deleteProject: (id: string) => request<void>('DELETE', projectPath(id)),
  addMember: (projectId: string, username: string) =>
    request<void>('POST', `${projectPath(projectId)}/members`, { username }),
  removeMember: (projectId: string, username: string) =>
    request<void>('DELETE', `${projectPath(projectId)}/members/${segment(username)}`),
  swarms: (projectId: string) => request<AccessSwarm[]>('GET', `${projectPath(projectId)}/swarms`),
  createSwarm: (projectId: string, name: string) =>
    request<AccessSwarm>('POST', `${projectPath(projectId)}/swarms`, { name }),
  deleteSwarm: (id: string) => request<void>('DELETE', swarmPath(id)),
  transferSwarm: (id: string, username: string) =>
    request<void>('POST', `${swarmPath(id)}/transfer`, { username }),
};
