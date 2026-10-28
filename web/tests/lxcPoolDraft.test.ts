import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { newPoolDraft, poolDraft, poolPayload, validatePoolDraft, workerPoolImages } from '@/features/substrate/workerPoolDraft';
import type { WorkerPool } from '@/services/brood/substrate';

const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');
const gvisor = `registry/gvisor@sha256:${'a'.repeat(64)}`;
const lxc = `registry/lxc@sha256:${'b'.repeat(64)}`;

// FEATURE: Явно установленный LXC-класс в панели пулов (#73).
// SPEC: Нет неявного включения LXC; образ соответствует карте Go API,
// runtime/configRef/UID существующего пула неизменяемы, ограничения видны во всех локалях.
test('offers installed LXC with its own image while keeping gvisor first and default', () => {
  const images = workerPoolImages({ workerImage: gvisor, workerImages: { lxc, gvisor } });
  expect(Object.keys(images)).toEqual(['gvisor', 'lxc']);
  const draft = { ...newPoolDraft(images.lxc, 'lxc'), name: 'trusted-lxc' };
  expect(validatePoolDraft(draft)).toEqual({});
  expect(poolPayload(null, draft).spec).toEqual({ replicas: 1, workerImage: lxc, sandboxClasses: [{ name: 'lxc' }] });
  expect(newPoolDraft(gvisor).sandboxClass).toBe('gvisor');
  expect(workerPoolImages({ workerImage: gvisor })).toEqual({ gvisor });
  expect(workerPoolImages({ workerImage: gvisor, workerImages: {} })).toEqual({});
  expect(validatePoolDraft({ ...draft, sandboxClass: 'fullvm' })).toHaveProperty('sandboxClass');
});

test('LXC pool edits preserve runtime, image, config and concurrency preconditions', () => {
  const pool: WorkerPool = { metadata: { name: 'trusted-lxc', uid: 'own-pool', resourceVersion: '7' }, spec: {
    replicas: 1, workerImage: lxc, sandboxClasses: [{ name: 'lxc', configRef: { name: 'trusted' } }],
  } };
  const draft = poolDraft(pool);
  expect(validatePoolDraft(draft)).toEqual({});
  const result = poolPayload(pool, { ...draft, sandboxClass: 'microvm', workerImage: gvisor, replicas: '0' });
  expect(result.metadata).toEqual(pool.metadata);
  expect(result.spec.workerImage).toBe(lxc);
  expect(result.spec.sandboxClasses).toEqual(pool.spec.sandboxClasses);
  expect(result.spec.replicas).toBe(0);
  expect(pool.spec.replicas).toBe(1);
});

test('shows LXC limitations for loaded pools and selected drafts in every locale', () => {
  const page = source('features/substrate/WorkerPoolsPage.tsx');
  expect(page).toContain("draft.sandboxClass === 'lxc'");
  expect(page.match(/t\('worker_pools.lxc_warning'\)/g)?.length).toBe(2);
  for (const locale of ['en', 'ru', 'zh-CN', 'zh-TW', 'ko', 'vi']) {
    const translations = JSON.parse(source(`i18n/locales/${locale}.json`));
    expect(typeof translations.worker_pools.lxc_warning).toBe('string');
    expect(translations.worker_pools.lxc_warning.length).toBeGreaterThan(40);
  }
});
