import { isAlias, parseDocument, visit } from 'yaml';
import { request } from './http';

export interface WorkflowPolicyRule {
  id: string;
  paths?: string[];
  actions?: string[];
  require: 'human' | 'independent_review' | 'deny';
  reviewRole?: string;
}

export interface ProjectWorkflowPolicy {
  projectId: string;
  revision: number;
  rules: WorkflowPolicyRule[];
  createdBy: string;
  createdAt: string;
}

const INVALID = 'Invalid project workflow policy';
const record = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value);
const nonempty = (value: unknown): value is string => typeof value === 'string' && value.length > 0;
const strings = (value: unknown): value is string[] =>
  Array.isArray(value) && value.every(nonempty);
const revisionValid = (value: unknown): value is number =>
  Number.isSafeInteger(value) && (value as number) >= 0;
const ruleKeys = new Set(['id', 'paths', 'actions', 'require', 'reviewRole']);

function ruleValid(value: unknown): value is WorkflowPolicyRule {
  if (!record(value) || Object.keys(value).some((key) => !ruleKeys.has(key)) || !nonempty(value.id))
    return false;
  if (
    value.require !== 'human' &&
    value.require !== 'independent_review' &&
    value.require !== 'deny'
  )
    return false;
  if (value.paths !== undefined && !strings(value.paths)) return false;
  if (value.actions !== undefined && !strings(value.actions)) return false;
  if (!(
    (value.paths as string[] | undefined)?.length || (value.actions as string[] | undefined)?.length
  ))
    return false;
  if (value.reviewRole !== undefined && typeof value.reviewRole !== 'string') return false;
  return value.require !== 'independent_review' || nonempty(value.reviewRole);
}

function rulesValid(value: unknown): value is WorkflowPolicyRule[] {
  return (
    Array.isArray(value) &&
    value.length <= 64 &&
    value.every(ruleValid) &&
    new Set(value.map((rule) => rule.id)).size === value.length
  );
}

export function parseProjectWorkflowPolicy(value: unknown): ProjectWorkflowPolicy {
  if (
    !record(value) ||
    !nonempty(value.projectId) ||
    !revisionValid(value.revision) ||
    !rulesValid(value.rules) ||
    typeof value.createdBy !== 'string' ||
    typeof value.createdAt !== 'string'
  )
    throw new Error(INVALID);
  return value as unknown as ProjectWorkflowPolicy;
}

export function parseWorkflowPolicyDraft(text: string): WorkflowPolicyRule[] | null {
  if (text.length > 1 << 20) return null;
  try {
    const doc = parseDocument(text, { uniqueKeys: true });
    if (doc.errors.length > 0 || doc.warnings.length > 0) return null;
    visit(doc, (_key, node) => {
      if (isAlias(node)) throw new Error(INVALID);
    });
    const value: unknown = doc.toJS({ maxAliasCount: 0 });
    return rulesValid(value) ? value : null;
  } catch {
    return null;
  }
}

const path = (projectId: string) =>
  `/api/projects/${encodeURIComponent(projectId)}/workflow-policy`;
function scoped(value: unknown, projectId: string): ProjectWorkflowPolicy {
  const policy = parseProjectWorkflowPolicy(value);
  if (policy.projectId !== projectId) throw new Error(INVALID);
  return policy;
}

export const workflowPolicyApi = {
  get: async (projectId: string): Promise<ProjectWorkflowPolicy> =>
    scoped(await request<unknown>('GET', path(projectId)), projectId),
  set: async (
    projectId: string,
    revision: number,
    rules: WorkflowPolicyRule[]
  ): Promise<ProjectWorkflowPolicy> => {
    if (!revisionValid(revision) || !rulesValid(rules)) throw new Error(INVALID);
    return scoped(await request<unknown>('PUT', path(projectId), { revision, rules }), projectId);
  },
};
