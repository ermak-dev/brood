// FEATURE: Создание рабочего пула Proxmox из формы.
// SPEC: Явные координаты и credentials, PEM в несколько строк, общий бюджет
// CPU/GiB и закреплённый шаблон; нецелые и переполненные значения отвергаются.
import { expect, test } from 'bun:test';
import { connectionPayload, newConnectionDraft, validateConnectionDraft } from '@/features/substrate/connectionDraft';

const valid = () => ({
  ...newConnectionDraft(), name: 'pve', endpoint: 'https://pve.invalid:8006',
  tokenID: 'worker@pve!brood', tokenSecret: 'synthetic-secret',
  caPEM: '-----BEGIN CERTIFICATE-----\nYWJj\n-----END CERTIFICATE-----',
  node: 'node-a', resourcePool: 'brood', storage: 'local-lvm', network: 'vmbr0',
  template: 'local:vztmpl/brood-claude-proxmox-abc.tar', templateDigest: `sha256:${'a'.repeat(64)}`,
  actors: '2', cpu: '4', memoryGiB: '4', diskGiB: '32',
});

test('an empty installation has no machine-specific form defaults', () => {
  const draft = newConnectionDraft();
  expect(draft.node).toBe('');
  expect(draft.endpoint).toBe('');
  expect(Object.keys(validateConnectionDraft(draft)).length).toBeGreaterThan(0);
});

test('the form serializes total capacity in API units without altering the secret or PEM', () => {
  const draft = valid();
  expect(validateConnectionDraft(draft)).toEqual({});
  const payload = connectionPayload(draft);
  expect(payload.capacity).toEqual({ actors: 2, cpuMillicores: '4000', memoryBytes: '4294967296', diskGiB: 32 });
  expect(payload.tokenSecret).toBe(draft.tokenSecret);
  expect(payload.caPEM).toBe(draft.caPEM);
  expect(payload.templateDigest).toBe(draft.templateDigest);
});

test('unsafe origins, invalid names and header injection cannot be submitted', () => {
  for (const [field, values] of Object.entries({
    endpoint: ['http://pve.invalid', 'https://u:p@pve.invalid', 'https://pve.invalid/path', 'https://pve.invalid?x', 'https://pve.invalid:99999'],
    name: ['Bad Name', '-pve', 'a'.repeat(49)], tokenID: ['invalid'], tokenSecret: ['secret\r\nX: injected'],
    templateDigest: ['latest'], caPEM: ['not a certificate'],
  })) {
    for (const value of values) expect(validateConnectionDraft({ ...valid(), [field]: value })[field]).toBeTruthy();
  }
});

test('invalid and insufficient resource budgets are rejected rather than rounded', () => {
  for (const field of ['actors', 'cpu', 'memoryGiB', 'diskGiB'] as const) {
    for (const value of ['0', '-1', '1.1', 'NaN', '1e3', '999999999999999999999']) {
      expect(validateConnectionDraft({ ...valid(), [field]: value })[field]).toBeTruthy();
    }
  }
  expect(validateConnectionDraft({ ...valid(), actors: '8' }).cpu).toBeTruthy();
  expect(() => connectionPayload({ ...valid(), cpu: 'bad' })).toThrow();
});
