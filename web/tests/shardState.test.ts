import { expect, test } from 'bun:test';
import type { AccessSwarm, AccessUser, SwarmMember } from '../src/services/brood/access';
import {
  capacityDraft,
  capacityFromDraft,
  canRemoveGrant,
  grantCandidates,
  swarmCapabilities,
  projectSelectionPath,
  retainedSwarm,
} from '../src/features/access/shardState';

const base: AccessSwarm = {
  id: 'swarm', project_id: 'project', owner_id: 'user', owner_username: 'owner', role: 'owner',
  name: 'swarm', atespace_name: 'swarm-native', state: 'active', created_at: '', updated_at: '',
};

// FEATURE: Role-aware controls and exact nullable shard settings (#53).
// SPEC: Viewer never gets write controls, editor never gets owner controls;
// nil differs from zero, int64 never passes through Number, last owner is retained.
test('derives workload and ownership controls from explicit role and state', () => {
  expect(swarmCapabilities(base, false)).toEqual({ manage: true, own: true, remove: true, retryCreate: false });
  expect(swarmCapabilities({ ...base, role: 'editor' }, false)).toEqual({ manage: true, own: false, remove: false, retryCreate: false });
  expect(swarmCapabilities({ ...base, role: 'viewer' }, false)).toEqual({ manage: false, own: false, remove: false, retryCreate: false });
  expect(swarmCapabilities({ ...base, role: undefined } as unknown as AccessSwarm, false).manage).toBe(false);
  expect(swarmCapabilities({ ...base, role: 'viewer' }, true).own).toBe(true);
  expect(swarmCapabilities({ ...base, state: 'error', retry: 'delete' }, false)).toEqual({ manage: false, own: false, remove: true, retryCreate: false });
  expect(swarmCapabilities({ ...base, state: 'error', retry: 'create' }, false).retryCreate).toBe(true);
  expect(swarmCapabilities({ ...base, state: 'deleted' }, true).remove).toBe(false);
});

test('selected swarm drafts survive transient refresh but reconcile confirmed access loss', () => {
  expect(retainedSwarm(base, { data: null, loading: true, error: '' })).toBe(base);
  expect(retainedSwarm(base, { data: null, loading: false, error: 'unavailable' })).toBe(base);
  expect(retainedSwarm(base, { data: [], loading: false, error: '' })).toBeNull();
  const demoted = { ...base, role: 'viewer' as const };
  expect(retainedSwarm(base, { data: [demoted], loading: false, error: '' })).toBe(demoted);
  expect(retainedSwarm(null, { data: [base], loading: false, error: '' })).toBeNull();
});

test('project switching keeps detail URLs consistent with the global context', () => {
  expect(projectSelectionPath('/projects/old', 'new/id')).toBe('/projects/new%2Fid');
  expect(projectSelectionPath('/projects/old/', 'new')).toBe('/projects/new');
  for (const path of ['/projects', '/actors', '/swarms', '/templates']) {
    expect(projectSelectionPath(path, 'new')).toBeNull();
  }
});

test('preserves nullable zero and full int64 capacity through an editor roundtrip', () => {
  const capacity = { worker_selector: { shard: 'one' }, max_swarms: null, max_actors: '0', max_cpu_millicores: '2000', max_memory_bytes: '9223372036854775807' };
  const draft = capacityDraft(capacity);
  expect(draft.max_swarms).toBe('');
  expect(draft.max_actors).toBe('0');
  expect(capacityFromDraft(draft)).toEqual(capacity);
  for (const invalid of ['-1', '1.5', '1e3', '9223372036854775808', '+1']) {
    expect(capacityFromDraft({ ...draft, max_actors: invalid })).toBeNull();
  }
  expect(capacityFromDraft({ ...draft, max_actors: ' ' })?.max_actors).toBeNull();
});

test('rejects invalid, duplicate or oversized worker selectors without dropping empty label values', () => {
  const draft = capacityDraft({ worker_selector: {}, max_swarms: null, max_actors: null, max_cpu_millicores: null, max_memory_bytes: null });
  expect(capacityFromDraft({ ...draft, labels: [{ key: 'shard', value: '' }] })?.worker_selector).toEqual({ shard: '' });
  for (const labels of [
    [{ key: '', value: 'one' }],
    [{ key: 'bad key', value: 'one' }],
    [{ key: 'shard', value: 'bad value' }],
    [{ key: 'shard', value: 'one' }, { key: 'shard', value: 'two' }],
    Array.from({ length: 11 }, (_, i) => ({ key: `label-${i}`, value: 'one' })),
  ]) expect(capacityFromDraft({ ...draft, labels })).toBeNull();
});

test('role choices exclude disabled and existing members, while last owner removal stays disabled', () => {
  const members: AccessUser[] = [
    { username: 'owner', display_name: 'Owner', is_admin: false, disabled: false },
    { username: 'editor', display_name: 'Editor', is_admin: false, disabled: false },
    { username: 'blocked', display_name: '', is_admin: false, disabled: true },
  ];
  const grants: SwarmMember[] = [{ user_id: 'owner-id', username: 'owner', display_name: 'Owner', role: 'owner', disabled: false }];
  expect(grantCandidates(members, grants).map((option) => option.value)).toEqual(['editor']);
  expect(canRemoveGrant(grants, 'owner')).toBe(false);
  expect(canRemoveGrant([...grants, { user_id: 'editor-id', username: 'editor', display_name: 'Editor', role: 'owner', disabled: false }], 'owner')).toBe(true);
  expect(canRemoveGrant([{ ...grants[0], role: 'viewer' }], 'owner')).toBe(true);
});
