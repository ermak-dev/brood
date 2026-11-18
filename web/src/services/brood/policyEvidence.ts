import { request } from './http';

export interface PolicyCandidate {
  id: string;
  version: 1;
  swarmId: string;
  sequence: number;
  pass: number;
  policyRevision: number;
  boundary: string;
  createdAt: string;
  target: string;
  terminal?: boolean;
  targetKind: string;
  initial: boolean;
  actionsKnown: boolean;
  actions: string[];
  rules: Array<{
    id: string;
    require: string;
    paths?: string[];
    actions?: string[];
    reviewRole?: string;
  }>;
  evidence: {
    id: string;
    known: boolean;
    complete: boolean;
    artifact: string;
    producerUid: string;
    paths: string[];
  };
  decision: {
    state: string;
    matches: Array<{ ruleId: string; paths: string[] }>;
    human: string[];
    review: string[];
    denied: string[];
    unknown: string[];
    notApplicable: string[];
  };
}

const INVALID = 'Invalid policy evidence response';
const record = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value);
const strings = (value: unknown): value is string[] =>
  Array.isArray(value) && value.every((item) => typeof item === 'string');
const nonempty = (value: unknown) => typeof value === 'string' && value.length > 0;
const positive = (value: unknown) => Number.isSafeInteger(value) && (value as number) > 0;

export function parsePolicyCandidate(value: unknown): PolicyCandidate {
  if (
    !record(value) ||
    value.version !== 1 ||
    !['id', 'swarmId', 'boundary', 'createdAt', 'targetKind'].every((key) =>
      nonempty(value[key])
    ) ||
    (value.terminal !== undefined && typeof value.terminal !== 'boolean') ||
    (value.terminal === true
      ? value.target !== '' ||
        value.targetKind !== 'exit' ||
        value.initial !== false ||
        value.boundary !== 'result'
      : !nonempty(value.target)) ||
    !['sequence', 'pass', 'policyRevision'].every((key) => positive(value[key])) ||
    typeof value.initial !== 'boolean' ||
    typeof value.actionsKnown !== 'boolean' ||
    !strings(value.actions) ||
    !Array.isArray(value.rules)
  )
    throw new Error(INVALID);
  for (const rule of value.rules) {
    if (
      !record(rule) ||
      !nonempty(rule.id) ||
      !nonempty(rule.require) ||
      (rule.paths !== undefined && !strings(rule.paths)) ||
      (rule.actions !== undefined && !strings(rule.actions)) ||
      (rule.reviewRole !== undefined && typeof rule.reviewRole !== 'string')
    )
      throw new Error(INVALID);
  }
  const evidence = value.evidence;
  if (
    !record(evidence) ||
    !['id', 'artifact', 'producerUid'].every((key) => typeof evidence[key] === 'string') ||
    typeof evidence.known !== 'boolean' ||
    typeof evidence.complete !== 'boolean' ||
    !strings(evidence.paths)
  )
    throw new Error(INVALID);
  const decision = value.decision;
  if (
    !record(decision) ||
    !nonempty(decision.state) ||
    !['human', 'review', 'denied', 'unknown', 'notApplicable'].every((key) =>
      strings(decision[key])
    ) ||
    !Array.isArray(decision.matches) ||
    !decision.matches.every(
      (match) => record(match) && nonempty(match.ruleId) && strings(match.paths)
    )
  )
    throw new Error(INVALID);
  return value as unknown as PolicyCandidate;
}

export const policyEvidenceApi = {
  detail: async (swarmId: string, requestId: string): Promise<PolicyCandidate> => {
    if (!swarmId || !requestId.startsWith('policy:') || requestId === 'policy:')
      throw new Error(INVALID);
    const candidate = parsePolicyCandidate(
      await request<unknown>(
        'GET',
        `/api/swarms/${encodeURIComponent(swarmId)}/requests/${encodeURIComponent(requestId)}/policy`
      )
    );
    if (candidate.swarmId !== swarmId || requestId !== `policy:${candidate.id}`)
      throw new Error(INVALID);
    return candidate;
  },
};
