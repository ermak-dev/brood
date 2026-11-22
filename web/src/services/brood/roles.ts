import { request } from './http';

export const ROLE_PERMISSIONS = [
  'agents',
  'diagnose',
  'send:agents',
  'send:operator',
  'artifact_get',
  'artifact_publish',
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

const rolesPath = '/api/roles';
const rolePath = (id: string) => `${rolesPath}/${encodeURIComponent(id)}`;

export const rolesApi = {
  list: async () =>
    ((await request<Role[]>('GET', rolesPath)) ?? []).map((role) => ({
      ...role,
      permissions: role.permissions ?? [],
      agents: role.agents ?? [],
    })),
  create: (body: RoleInput) => request<unknown>('POST', rolesPath, body),
  update: (id: string, body: RoleInput) => request<unknown>('PUT', rolePath(id), body),
  remove: (id: string) => request<void>('DELETE', rolePath(id)),
};
