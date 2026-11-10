import type { AccessSwarm, AccessUser, ShardCapacity, SwarmMember } from '@/services/brood/access';
import { userOption } from './state';

/** Куда перейти при смене проекта в шапке: детали проекта — к деталям
 * нового, страница роя — к списку роёв нового проекта (рой принадлежит
 * одному проекту, #215). Остальные разделы и так следуют контексту. */
export function projectSelectionPath(pathname: string, project: string): string | null {
  if (/^\/projects\/[^/]+\/?$/.test(pathname)) return `/projects/${encodeURIComponent(project)}`;
  return /^\/swarms\/[^/]+\/?$/.test(pathname) ? '/swarms' : null;
}

export function swarmCapabilities(swarm: AccessSwarm, administrator: boolean) {
  const owner = administrator || swarm.role === 'owner';
  const active = swarm.state === 'active';
  return {
    manage: active && (owner || swarm.role === 'editor'),
    own: active && owner,
    remove: owner && ['active', 'error', 'deleting'].includes(swarm.state),
    retryCreate:
      owner &&
      (swarm.state === 'provisioning' || (swarm.state === 'error' && swarm.retry === 'create')),
  };
}

export function grantCandidates(members: AccessUser[] | null, grants: SwarmMember[] | null) {
  if (!members || !grants) return [];
  const assigned = new Set(grants.map((grant) => grant.username));
  return members
    .filter((member) => !member.disabled && !assigned.has(member.username))
    .map(userOption);
}

export function canRemoveGrant(members: SwarmMember[] | null, username: string): boolean {
  const member = members?.find((item) => item.username === username);
  if (!member || !members) return false;
  return member.role !== 'owner' || members.filter((item) => item.role === 'owner').length > 1;
}

const limitKeys = ['max_swarms', 'max_actors', 'max_cpu_millicores', 'max_memory_bytes'] as const;
type LimitKey = (typeof limitKeys)[number];
export interface CapacityDraft extends Record<LimitKey, string> {
  labels: Array<{ key: string; value: string }>;
}

export function capacityDraft(capacity: ShardCapacity): CapacityDraft {
  return {
    max_swarms: capacity.max_swarms ?? '',
    max_actors: capacity.max_actors ?? '',
    max_cpu_millicores: capacity.max_cpu_millicores ?? '',
    max_memory_bytes: capacity.max_memory_bytes ?? '',
    labels: Object.entries(capacity.worker_selector).map(([key, value]) => ({ key, value })),
  };
}

const labelPart = /^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$/;
const dnsPart = /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/;
function validLabel(key: string, value: string) {
  const parts = key.split('/');
  const name = parts[parts.length - 1];
  if (parts.length > 2 || name.length > 63 || !labelPart.test(name)) return false;
  if (
    parts.length === 2 &&
    (parts[0].length > 253 ||
      !parts[0].split('.').every((part) => part.length <= 63 && dnsPart.test(part)))
  )
    return false;
  return value.length <= 63 && (value === '' || labelPart.test(value));
}

export function capacityFromDraft(draft: CapacityDraft): ShardCapacity | null {
  const result: ShardCapacity = {
    worker_selector: {},
    max_swarms: null,
    max_actors: null,
    max_cpu_millicores: null,
    max_memory_bytes: null,
  };
  for (const key of limitKeys) {
    const value = draft[key].trim();
    if (value === '') continue;
    if (
      value.length > 19 ||
      !/^(?:0|[1-9][0-9]*)$/.test(value) ||
      BigInt(value) > 9223372036854775807n
    )
      return null;
    result[key] = value;
  }
  const labels = draft.labels
    .map(({ key, value }) => ({ key: key.trim(), value: value.trim() }))
    .filter(({ key, value }) => key !== '' || value !== '');
  if (labels.length > 10) return null;
  const used = new Set<string>();
  for (const { key, value } of labels) {
    if (!validLabel(key, value) || used.has(key)) return null;
    used.add(key);
    result.worker_selector[key] = value;
  }
  return result;
}
