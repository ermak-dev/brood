import { describe, expect, test } from 'bun:test';
import { newPoolDraft, poolDraft, poolPayload, validatePoolDraft } from '@/features/substrate/workerPoolDraft';
import { isLabelKey, isLabelValue } from '@/features/substrate/format';
import type { WorkerPool } from '@/services/brood/substrate';

// BUG: Форма пула не имеет поля меток, поэтому созданный из панели пул
// не совпадает ни с одним workerSelector и не принимает акторов (#88).
// HYPOTHESIS: Черновик пула не переносит metadata.labels ни в одну сторону.

const image = `registry/worker@sha256:${'a'.repeat(64)}`;

const existing = (): WorkerPool => ({
  apiVersion: 'ate.dev/v1',
  kind: 'WorkerPool',
  metadata: { name: 'pool-one', uid: 'u1', resourceVersion: '7', labels: { 'brood.dev/purpose': 'development' } },
  spec: { replicas: 1, workerImage: image, sandboxClasses: [{ name: 'gvisor' }] },
});

describe('pool labels reach the object that placement reads', () => {
  test('a new pool starts without labels and writes none', () => {
    const draft = { ...newPoolDraft(image), name: 'fresh' };
    expect(draft.labels).toEqual({});
    expect(poolPayload(null, draft).metadata.labels).toBeUndefined();
  });

  test('labels typed into the form reach metadata.labels', () => {
    const draft = { ...newPoolDraft(image), name: 'fresh', labels: { 'brood.dev/purpose': 'development' } };
    expect(poolPayload(null, draft).metadata.labels).toEqual({ 'brood.dev/purpose': 'development' });
  });

  test('an existing pool round-trips its labels unchanged', () => {
    const source = existing();
    const draft = poolDraft(source);
    expect(draft.labels).toEqual({ 'brood.dev/purpose': 'development' });
    expect(poolPayload(source, draft).metadata.labels).toEqual({ 'brood.dev/purpose': 'development' });
  });

  test('clearing every label removes the field instead of sending an empty map', () => {
    const source = existing();
    const payload = poolPayload(source, { ...poolDraft(source), labels: {} });
    expect(payload.metadata.labels).toBeUndefined();
    expect(payload.metadata.uid).toBe('u1');
    expect(payload.metadata.resourceVersion).toBe('7');
  });

  test('editing labels leaves the rest of the pool alone', () => {
    const source = existing();
    const payload = poolPayload(source, { ...poolDraft(source), labels: { tier: 'trusted' } });
    expect(payload.spec.workerImage).toBe(image);
    expect(payload.spec.sandboxClasses).toEqual([{ name: 'gvisor' }]);
  });
});

describe('label syntax is rejected before the request, with the offending field named', () => {
  test('Kubernetes label keys and values are accepted exactly as Kubernetes accepts them', () => {
    for (const key of ['brood.dev/purpose', 'purpose', 'a', 'a_b.c-d', `x/${'a'.repeat(63)}`])
      expect(isLabelKey(key)).toBe(true);
    for (const key of ['', '/purpose', 'a/b/c', `${'a'.repeat(64)}`, '-bad', 'bad-', 'UP/..'])
      expect(isLabelKey(key)).toBe(false);
    for (const value of ['', 'development', 'A_b.c-1', 'a'.repeat(63)])
      expect(isLabelValue(value)).toBe(true);
    for (const value of ['a'.repeat(64), '-bad', 'bad-', 'has space'])
      expect(isLabelValue(value)).toBe(false);
  });

  test('a malformed label fails validation and a correct one passes', () => {
    const draft = { ...newPoolDraft(image), name: 'fresh' };
    expect(validatePoolDraft({ ...draft, labels: { 'bad key': 'x' } })).toHaveProperty('labels');
    expect(validatePoolDraft({ ...draft, labels: { purpose: 'has space' } })).toHaveProperty('labels');
    expect(validatePoolDraft({ ...draft, labels: { 'brood.dev/purpose': 'development' } }).labels).toBeUndefined();
  });
});
