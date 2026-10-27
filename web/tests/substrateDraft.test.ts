import { describe, expect, test } from 'bun:test';
import {
  bytesToMib,
  formatCpu,
  formatMemory,
  fromPairs,
  isShortName,
  mibToBytes,
  shortState,
  toneOf,
  toPairs,
} from '@/features/substrate/format';
import { asJson, cleaned, copyOf, shapeProblem } from '@/features/substrate/templateDraft';
import type { ActorTemplate } from '@/services/brood/substrate';

// brood fork: pure helpers of the Substrate entity pages -- protojson enums and
// sizes, label maps, the name rule ateapi enforces, and the template draft the
// editor builds and validates before the form reads it.

const template = (): ActorTemplate => ({
  metadata: { atespace: 'demo', name: 'claude', uid: 'u-1', version: '3' },
  containers: [
    {
      name: 'claude',
      image: 'busybox@sha256:abc',
      env: [
        { name: 'MODE', value: 'e2e' },
        { name: ' ', value: 'draft row' },
      ],
      volumeMounts: [{ name: '', mountPath: '/draft' }],
      wakeupProbe: { httpGet: { port: 8080 } },
    },
  ],
  volumes: [{ name: 'work', durableDir: {} }, { name: '' }],
  snapshotConfig: { preferredFidelity: 'SNAPSHOT_FIDELITY_MEMORY', goldenSnapshotConfig: { mode: 'GOLDEN_SNAPSHOT_MODE_DISABLED' } },
  status: { goldenSnapshotStatus: { errorMessage: 'boom' } },
});

describe('substrate format', () => {
  test('ateapi short names', () => {
    for (const ok of ['brood', 'a', 'team-1', 'x'.repeat(63)]) expect(isShortName(ok)).toBe(true);
    for (const bad of ['', 'Brood', '-a', 'a-', 'a_b', 'a.b', 'x'.repeat(64)]) expect(isShortName(bad)).toBe(false);
  });

  test('enum names and tones', () => {
    expect(shortState('ACTOR_STATE_RUNNING', 'ACTOR_STATE_')).toBe('running');
    expect(shortState(undefined, 'ACTOR_STATE_')).toBe('');
    expect(toneOf('running')).toBe('success');
    expect(toneOf('crashed')).toBe('danger');
    expect(toneOf('something-new')).toBe('muted');
  });

  test('int64 sizes from protojson strings, unit kept with the number', () => {
    expect(formatCpu('1500')).toBe('1.5 CPU');
    expect(formatCpu('250')).toBe('250m');
    expect(formatCpu(undefined)).toBe('—');
    expect(formatMemory('2147483648')).toBe('2 GiB');
    expect(formatMemory(String(512 * 1024 * 1024))).toBe('512 MiB');
    expect(mibToBytes('512')).toBe(String(512 * 1024 * 1024));
    expect(mibToBytes('  ')).toBeUndefined();
    expect(bytesToMib(String(512 * 1024 * 1024))).toBe('512');
  });

  test('label pairs drop rows without a key', () => {
    expect(fromPairs([{ key: ' workload ', value: 'claude' }, { key: '', value: 'draft' }])).toEqual({
      workload: 'claude',
    });
    expect(toPairs({ a: '1' })).toEqual([{ key: 'a', value: '1' }]);
    expect(toPairs(undefined)).toEqual([]);
  });
});

describe('template draft', () => {
  test('a copy keeps every field but identity and status', () => {
    const copy = copyOf(template());
    expect(copy.metadata).toEqual({ name: 'claude-copy' });
    expect(copy.status).toBeUndefined();
    expect(copy.containers[0].wakeupProbe).toEqual({ httpGet: { port: 8080 } });
    expect(copy.snapshotConfig?.goldenSnapshotConfig).toEqual({ mode: 'GOLDEN_SNAPSHOT_MODE_DISABLED' });
  });

  test('a copy of a long name stays a valid name length', () => {
    const tpl = template();
    tpl.metadata.name = 'x'.repeat(63);
    expect(copyOf(tpl).metadata.name.length).toBe(63);
  });

  test('what goes to ateapi: rows without a name dropped, unknown fields kept', () => {
    const out = cleaned(template());
    expect(out.metadata).toEqual({ name: 'claude' });
    expect(out.status).toBeUndefined();
    expect(out.containers[0].env).toEqual([{ name: 'MODE', value: 'e2e' }]);
    expect(out.containers[0].volumeMounts).toEqual([]);
    expect(out.volumes).toEqual([{ name: 'work', durableDir: {} }]);
    expect(out.containers[0].wakeupProbe).toEqual({ httpGet: { port: 8080 } });
    expect(JSON.parse(asJson(template()))).toEqual(out);
  });

  test('a JSON draft the form can read passes', () => {
    expect(shapeProblem(cleaned(template()))).toBeNull();
    expect(shapeProblem({ containers: [{ name: 'main', image: 'i' }] })).toBeNull();
  });

  test('a JSON draft that would break the form is refused with the field', () => {
    // Each of these used to reach .trim() on undefined and blank the app.
    expect(shapeProblem({ containers: [{ name: 'main' }] })).toBe('containers[0]: name, image');
    expect(shapeProblem({ containers: [{ name: 'main', image: 'i', env: { MODE: 'x' } }] })).toBe(
      'containers[0].env'
    );
    expect(shapeProblem({ containers: [{ name: 'main', image: 'i', args: [1] }] })).toBe('containers[0].args');
    expect(shapeProblem({ containers: [{ name: 'main', image: 'i' }], volumes: [{ durableDir: {} }] })).toBe(
      'volumes'
    );
    expect(shapeProblem({ containers: {} })).toBe('containers');
    expect(shapeProblem(null)).toBe('containers');
  });
});
