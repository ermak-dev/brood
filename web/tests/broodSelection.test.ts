import { expect, test } from 'bun:test';
import { useProjectStore } from '@/stores/useProjectStore';

test('swarm selection survives restoring the same user but never follows another user', () => {
  const selection = useProjectStore.getState();
  selection.bindUser('alice');
  selection.setProject('alice-swarm');
  useProjectStore.getState().bindUser('alice');
  expect(useProjectStore.getState().project).toBe('alice-swarm');
  useProjectStore.getState().bindUser('bob');
  expect(useProjectStore.getState().project).toBe('');
  useProjectStore.getState().setProject('bob-swarm');
  useProjectStore.getState().bindUser('');
  expect(useProjectStore.getState().project).toBe('');
});
