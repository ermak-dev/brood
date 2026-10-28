import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
const source = (path: string) => readFileSync(new URL(`../src/${path}`, import.meta.url), 'utf8');

// FEATURE: Настройки шарда и роли роя в существующей панели (#53).
// SPEC: Детали используют глобальный проект; ACL выбирает участников проекта,
// защищает последнего owner; лимиты редактирует только администратор.
test('swarm access uses project-member choices and protects the final owner', () => {
  const page = source('features/access/SwarmAccess.tsx');
  expect(page).toContain('accessApi.swarmMembers(swarm.id)');
  expect(page).toContain('accessApi.members(swarm.project_id)');
  expect(page).toContain('grantCandidates(');
  expect(page).toContain('canRemoveGrant(');
  expect(page).toContain('swarmCapabilities(swarm, admin).own');
  expect(page).toContain('accessApi.setSwarmMember(');
  expect(page).toContain("type: 'select'");
});

test('editors preserve drafts through refresh and label controls do not submit', () => {
  const capacity = source('features/access/CapacityEditor.tsx');
  expect(capacity.match(/type="button"/g)?.length).toBe(2);
  const detail = source('features/access/ProjectDetailPage.tsx');
  expect(detail).toContain('useState<ShardCapacity | null>(null)');
  expect(detail).toContain('capacity={editing}');
  const grants = source('features/access/SwarmAccess.tsx');
  expect(grants).toMatch(/\(!editing \|\| !canOwn\) &&/);
  const swarms = source('features/access/SwarmsPage.tsx');
  expect(swarms).toContain('retainedSwarm(selectedAccess, resource)');
});

test('project detail exposes exact usage and capacity without another project picker', () => {
  const page = source('features/access/ProjectDetailPage.tsx');
  expect(page).toContain('ProjectScope');
  expect(page).toContain('accessApi.project(project.id)');
  expect(page).toContain('usage_available');
  expect(page).toContain('ProjectMembers');
  expect(page).toContain('admin &&');
  expect(page).not.toContain('ProjectSelect');
  const editor = source('features/access/CapacityEditor.tsx');
  expect(editor).toContain('capacityFromDraft(draft)');
  expect(editor).toContain('capacityDraft(capacity)');
  expect(editor).not.toContain('Number(');
});
