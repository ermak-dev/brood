import { request } from './http';

export const ROLE_PERMISSIONS = [
  'agents',
  'diagnose',
  'send:agents',
  'send:operator',
  'tail:self',
  'tail:swarm',
  'add',
  'start',
  'stop',
  'restart',
  'slash',
  'reset',
  'rm',
] as const;

export const DELEGATED_ROLE_PERMISSIONS: ReadonlySet<string> = new Set([
  'tail:swarm',
  'add',
  'start',
  'stop',
  'restart',
  'slash',
  'reset',
  'rm',
]);

export interface RoleInput {
  name: string;
  prompt: string;
  permissions: string[];
}

export interface Role extends RoleInput {
  id: string;
  default: boolean;
  agents: string[];
}

const rolesPath = (project: string) => `/api/projects/${encodeURIComponent(project)}/roles`;
const rolePath = (project: string, id: string) => `${rolesPath(project)}/${encodeURIComponent(id)}`;

export const rolesApi = {
  list: async (project: string) =>
    ((await request<Role[]>('GET', rolesPath(project))) ?? []).map((role) => ({
      ...role,
      permissions: role.permissions ?? [],
      agents: role.agents ?? [],
    })),
  create: (project: string, body: RoleInput) => request<unknown>('POST', rolesPath(project), body),
  update: (project: string, id: string, body: RoleInput) =>
    request<unknown>('PUT', rolePath(project, id), body),
  remove: (project: string, id: string) => request<void>('DELETE', rolePath(project, id)),
};
