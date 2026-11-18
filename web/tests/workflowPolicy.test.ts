import { expect, test } from 'bun:test';
import {
  parseProjectWorkflowPolicy,
  parseWorkflowPolicyDraft,
} from '@/services/brood/workflowPolicy';

const rule = { id: 'storage', paths: ['go/**'], require: 'human' };

test('project policy parser preserves explicit zero and configured revisions', () => {
  const empty = { projectId: 'p', revision: 0, rules: [], createdBy: '', createdAt: '' };
  expect(parseProjectWorkflowPolicy(empty)).toEqual(empty);
  const configured = {
    ...empty,
    revision: 3,
    rules: [rule],
    createdBy: 'admin',
    createdAt: '2026-11-16T08:00:00Z',
  };
  expect(parseProjectWorkflowPolicy(configured)).toEqual(configured);
  for (const revision of [undefined, null, -1, 1.5, '3', Number.MAX_SAFE_INTEGER + 1]) {
    expect(() => parseProjectWorkflowPolicy({ ...configured, revision })).toThrow();
  }
  expect(() => parseProjectWorkflowPolicy({ ...configured, rules: null })).toThrow();
});

test('policy draft preserves rules and supports explicit empty policy', () => {
  expect(parseWorkflowPolicyDraft('- id: storage\n  paths: ["go/**"]\n  require: human')).toEqual([
    rule,
  ]);
  expect(parseWorkflowPolicyDraft('[]')).toEqual([]);
  expect(
    parseWorkflowPolicyDraft('[{"id":"storage","paths":["go/**"],"require":"human"}]')
  ).toEqual([rule]);
});

test('policy draft refuses duplicate keys, aliases, unknown fields and invalid obligation shapes', () => {
  for (const raw of [
    '',
    'null',
    '{}',
    '- id: storage\n  require: human\n  require: deny\n  paths: ["go/**"]',
    '- id: storage\n  require: human\n  paths: ["go/**"]\n  approved: true',
    '- id: review\n  require: independent_review\n  paths: ["go/**"]',
    '- id: storage\n  require: maybe\n  paths: ["go/**"]',
    '- &rule {id: storage, paths: ["go/**"], require: human}\n- *rule',
    '- id: storage\n  require: human\n  paths: [17]',
  ])
    expect(parseWorkflowPolicyDraft(raw)).toBeNull();
});
