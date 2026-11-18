// FEATURE: Панель различает явного владельца проекта и обычного member.
// SPEC: Неизвестная/ошибочная ownership не даёт прав; implicit revision 0 — не grant.
import { expect, test } from 'bun:test';
import { canEditProjectPolicy, parseProjectOwnership } from '@/services/brood/projectOwnership';

const unassigned = {
  projectId: 'project',
  viewerUid: 'alice-id',
  revision: 0,
  ownerUid: '',
  ownerUsername: '',
  updatedBy: '',
  updatedAt: '',
};
const assigned = {
  ...unassigned,
  revision: 1,
  ownerUid: 'alice-id',
  ownerUsername: 'alice',
  updatedBy: 'admin-id',
  updatedAt: '2026-11-18T10:00:00Z',
};

test('ownership parser preserves explicit unassigned, assigned and revoked revisions', () => {
  expect(parseProjectOwnership(unassigned)).toEqual(unassigned);
  expect(parseProjectOwnership(assigned)).toEqual(assigned);
  const revoked = { ...assigned, revision: 2, ownerUid: '', ownerUsername: '' };
  expect(parseProjectOwnership(revoked)).toEqual(revoked);
});

test('ownership parser rejects implied or malformed grants', () => {
  for (const value of [
    null,
    {},
    { ...unassigned, ownerUid: 'alice-id' },
    { ...assigned, revision: -1 },
    { ...assigned, revision: '1' },
    { ...assigned, revision: Number.MAX_SAFE_INTEGER + 1 },
    { ...assigned, ownerUsername: '' },
    { ...assigned, updatedBy: '' },
    { ...assigned, updatedAt: null },
    { ...assigned, viewerUid: '' },
    { ...assigned, viewerUid: undefined },
  ])
    expect(() => parseProjectOwnership(value)).toThrow();
});

test('policy editing requires current explicit owner identity or administrator', () => {
  expect(canEditProjectPolicy(false, 'alice-id', assigned)).toBe(true);
  expect(canEditProjectPolicy(false, 'bob-id', assigned)).toBe(false);
  expect(canEditProjectPolicy(false, 'alice-id', unassigned)).toBe(false);
  expect(canEditProjectPolicy(false, 'alice-id', undefined)).toBe(false);
  expect(canEditProjectPolicy(false, 'alice-id', assigned, true)).toBe(false);
  expect(canEditProjectPolicy(true, 'admin-id', undefined, true)).toBe(true);
});
