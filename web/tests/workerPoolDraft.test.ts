import { describe, expect, test } from 'bun:test';
import { newPoolDraft, poolDraft, poolPayload, validatePoolDraft } from '@/features/substrate/workerPoolDraft';
import * as drafts from '@/features/substrate/workerPoolDraft';
import type { WorkerPool } from '@/services/brood/substrate';

// FEATURE: WorkerPool drafts validate Kubernetes inputs without losing unread fields.
// SPEC: Installation-wide gvisor creation, pinned default image, nonnegative int32
// replicas, Kubernetes quantities, preserved uid/resourceVersion and unsupported fields.
// IMPLEMENTATION: Pure draft validation and field-level edits on a cloned Kubernetes object.
// RESULT: IMPLEMENTED — defaults and edits preserve the contract, including decimal unit equivalence.
// STATUS: DONE
const image = `registry/worker@sha256:${'a'.repeat(64)}`;
const existing = (): WorkerPool => ({
  apiVersion: 'ate.dev/v1', kind: 'WorkerPool',
  metadata: { name: 'pool.one', uid: 'u1', resourceVersion: '7', labels: { owner: 'ops' } },
  spec: {
    replicas: 2, workerImage: image,
    sandboxClasses: [{ name: 'microvm', configRef: { name: 'custom' } }],
    template: {
      nodeSelector: { zone: 'one' },
      resources: { requests: { cpu: '500m', memory: '1Gi', 'example.com/device': '1' }, limits: { cpu: '2', memory: '2Gi' }, claims: [{ name: 'device' }] },
    },
    futureField: true,
  },
  status: { replicas: 2, readyReplicas: 1 },
});

// FEATURE: Создание пулов с установленными gvisor и microvm.
// SPEC: Карта образов определяет выбор класса; старый ответ даёт только gvisor.
// Класс и образ существующего пула неизменяемы, включая configRef.
// IMPLEMENTATION: Карта установленных образов, выбор класса при создании и сохранение исходных полей.
// RESULT: IMPLEMENTED — microvm использует свой образ, старый контракт сохраняет gvisor.
// STATUS: DONE
describe('worker pool runtime controls', () => {
  const microvmImage = `registry/microvm@sha256:${'b'.repeat(64)}`;

  test('installed supported classes use their matching image, never fullvm', () => {
    const images = drafts.workerPoolImages({ workerImage: image, workerImages: {
      gvisor: image, microvm: microvmImage, fullvm: 'unsupported',
    } });
    expect(images).toEqual({ gvisor: image, microvm: microvmImage });
    const draft = { ...newPoolDraft(images.microvm, 'microvm'), name: 'microvm-pool' };
    expect(validatePoolDraft(draft)).toEqual({});
    expect(poolPayload(null, draft).spec).toEqual({
      replicas: 1, workerImage: microvmImage, sandboxClasses: [{ name: 'microvm' }],
    });
    expect(validatePoolDraft({ ...draft, sandboxClass: 'fullvm' })).toHaveProperty('sandboxClass');
  });

  test('legacy fallback applies only when the map is absent', () => {
    expect(drafts.workerPoolImages({ workerImage: image })).toEqual({ gvisor: image });
    expect(drafts.workerPoolImages({ workerImage: image, workerImages: {} })).toEqual({});
    expect(drafts.workerPoolImages({ workerImage: image, workerImages: { microvm: microvmImage } }))
      .toEqual({ microvm: microvmImage });
    expect(drafts.workerPoolImages({ workerImage: '', workerImages: { gvisor: '', fullvm: image } }))
      .toEqual({});
  });

  test('replica and resource edits cannot replace existing microvm image or class', () => {
    const source = existing();
    source.spec.workerImage = microvmImage;
    const draft = poolDraft(source);
    expect(draft.sandboxClass).toBe('microvm');
    expect(draft.workerImage).toBe(microvmImage);
    const result = poolPayload(source, {
      ...draft, replicas: '3', requestCpu: '1', workerImage: image, sandboxClass: 'gvisor',
    });
    expect(result.spec.workerImage).toBe(microvmImage);
    expect(result.spec.sandboxClasses).toEqual([{ name: 'microvm', configRef: { name: 'custom' } }]);
    expect(result.spec.replicas).toBe(3);
    expect(result.spec.template?.resources?.requests?.cpu).toBe('1');
    expect(source.spec.replicas).toBe(2);
  });
});

describe('worker pool draft', () => {
  test('new pools use the supplied deployed image and gvisor, without namespace', () => {
    const draft = { ...newPoolDraft(image), name: 'new-pool' };
    expect(validatePoolDraft(draft)).toEqual({});
    expect(poolPayload(null, draft)).toEqual({ metadata: { name: 'new-pool' }, spec: { replicas: 1, workerImage: image, sandboxClasses: [{ name: 'gvisor' }] } });
    expect(validatePoolDraft(newPoolDraft(''))).toHaveProperty('workerImage');
  });

  test('names follow Kubernetes DNS subdomain rules', () => {
    const draft = newPoolDraft(image);
    for (const name of ['a', 'pool.one', 'x'.repeat(63), `${'a'.repeat(63)}.b`]) {
      expect(validatePoolDraft({ ...draft, name })).not.toHaveProperty('name');
    }
    for (const name of ['', 'Pool', '-a', 'a-', 'a..b', 'a_b', 'x'.repeat(64), `${'a.'.repeat(127)}a`]) {
      expect(validatePoolDraft({ ...draft, name })).toHaveProperty('name');
    }
  });

  test('replicas accept zero and int32 maximum, not blanks, fractions or overflow', () => {
    const draft = { ...newPoolDraft(image), name: 'pool' };
    for (const replicas of ['0', '1', '2147483647']) expect(validatePoolDraft({ ...draft, replicas })).toEqual({});
    for (const replicas of ['', ' ', '-1', '1.5', '1e2', '2147483648', 'NaN']) {
      expect(validatePoolDraft({ ...draft, replicas })).toHaveProperty('replicas');
    }
  });

  test('optional Kubernetes quantities accept decimal, binary and exponent units', () => {
    const draft = { ...newPoolDraft(image), name: 'pool' };
    for (const quantity of ['', '0', '500m', '1.5', '.5', '2Gi', '128Mi', '1e3', '1G', '100u', '1n']) {
      expect(validatePoolDraft({ ...draft, requestCpu: quantity })).toEqual({});
    }
    for (const quantity of ['-1', 'banana', '1 Gi', '1gi', 'Infinity', '1MiB']) {
      expect(validatePoolDraft({ ...draft, requestMemory: quantity })).toHaveProperty('requestMemory');
    }
    expect(validatePoolDraft({ ...draft, requestCpu: '1500m', limitCpu: '1' })).toHaveProperty('limitCpu');
    expect(validatePoolDraft({ ...draft, requestMemory: '2Gi', limitMemory: '2000Mi' })).toHaveProperty('limitMemory');
    expect(validatePoolDraft({ ...draft, requestMemory: '1Gi', limitMemory: '1073741824' })).toEqual({});
  });

  test('equivalent decimal quantities are not rejected due to floating point multiplication', () => {
    const draft = { ...newPoolDraft(image), name: 'pool', requestCpu: '700m', limitCpu: '0.7' };
    expect(validatePoolDraft(draft)).toEqual({});
    expect(validatePoolDraft({ ...draft, requestCpu: '1K' })).toHaveProperty('requestCpu');
  });

  test('edits preserve identity, concurrency and unsupported fields without mutating the original', () => {
    const original = existing();
    const before = JSON.stringify(original);
    const draft = { ...poolDraft(original), replicas: '0', requestCpu: '', limitMemory: '3Gi' };
    const result = poolPayload(original, draft);
    expect(result.metadata).toEqual(original.metadata);
    expect(result.spec.sandboxClasses).toEqual(original.spec.sandboxClasses);
    expect(result.spec.futureField).toBe(true);
    expect(result.spec.template?.nodeSelector).toEqual({ zone: 'one' });
    expect(result.spec.template?.resources).toEqual({ requests: { memory: '1Gi', 'example.com/device': '1' }, limits: { cpu: '2', memory: '3Gi' }, claims: [{ name: 'device' }] });
    expect(result.spec.replicas).toBe(0);
    expect(result.status).toBeUndefined();
    expect(JSON.stringify(original)).toBe(before);
  });
});
