// BUG: Ephemeral executor попадает в список назначения постоянных ролей графа.
// SPEC: Класс не выводится из role/name; legacy без class остаётся persistent.
import { expect, test } from 'bun:test';
import { managedActors } from '@/services/brood/roles';
import { bindableActors } from '@/features/swarms/roleState';

const row = (uid: string, extra: Record<string, unknown> = {}) => ({
  provider: 'proxmox',
  metadata: { uid, name: uid },
  generation: '1',
  phase: 'running',
  desired: 'running',
  role: 'worker',
  ...extra,
});

test('role candidates exclude ephemeral and unknown classes without hiding persistent workers', () => {
  const actors = managedActors({
    results: [
      row('legacy'),
      row('persistent', { class: 'persistent' }),
      row('ephemeral', { class: 'ephemeral' }),
      row('future', { class: 'future' }),
    ],
  });
  expect(actors.map((actor) => actor.uid)).toEqual(['legacy', 'persistent']);
  expect(bindableActors(actors).map((actor) => actor.uid)).toEqual(['legacy', 'persistent']);
});

test('malformed present class is not silently interpreted as persistent', () => {
  for (const value of [null, '', [], ['persistent'], 1, {}]) {
    expect(() => managedActors({ results: [row('malformed', { class: value })] })).toThrow(
      'Invalid swarm actor list'
    );
  }
});
