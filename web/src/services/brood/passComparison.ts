import { request } from './http';

export type ComparisonStatus = 'same' | 'different' | 'unknown' | 'not_comparable';
export interface ComparisonRef {
  swarmId: string;
  pass: number;
}
export interface ComparisonCount {
  reference: number;
  candidate: number;
  delta: number;
}
export interface ComparedStep {
  sequence: number;
  nodeId: string;
  state: string;
  outcome?: string;
  report?: string;
  reportedOutcome?: string;
  reportedReport?: string;
  durationMillis?: number;
  childTasks: number;
}
export interface ComparedVisit {
  position: number;
  reference?: ComparedStep;
  candidate?: ComparedStep;
  node: 'same' | 'different' | 'missing';
  outcome: ComparisonStatus;
  report: ComparisonStatus;
  reportedOutcome: ComparisonStatus;
  reportedReport: ComparisonStatus;
  durationDeltaMillis?: number;
}
export interface PassComparison {
  version: 1;
  alignment: 'visit-position';
  reference: ComparisonRef;
  candidate: ComparisonRef;
  sameGoal: boolean;
  sameSource: boolean;
  sameDefinition: boolean;
  stepCount: ComparisonCount;
  childTaskCount: ComparisonCount;
  steps: ComparedVisit[];
}

const record = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value);
const integer = (value: unknown): value is number =>
  typeof value === 'number' && Number.isSafeInteger(value);
const positive = (value: unknown): value is number => integer(value) && value > 0;
const text = (value: unknown): value is string => typeof value === 'string' && value.length > 0;
function requireValue(condition: unknown): asserts condition {
  if (!condition) throw new Error('Invalid pass comparison response');
}
function count(value: unknown): ComparisonCount {
  requireValue(
    record(value) && integer(value.reference) && integer(value.candidate) && integer(value.delta)
  );
  requireValue(
    value.reference >= 0 &&
      value.candidate >= 0 &&
      value.reference <= 1 << 30 &&
      value.candidate <= 1 << 30 &&
      value.delta === value.candidate - value.reference
  );
  return value as unknown as ComparisonCount;
}
function ref(value: unknown): ComparisonRef {
  requireValue(record(value) && text(value.swarmId) && positive(value.pass));
  return value as unknown as ComparisonRef;
}
function step(value: unknown, previous: number): ComparedStep {
  requireValue(
    record(value) &&
      positive(value.sequence) &&
      value.sequence > previous &&
      text(value.nodeId) &&
      text(value.state)
  );
  requireValue(integer(value.childTasks) && value.childTasks >= 0 && value.childTasks <= 1 << 30);
  for (const key of ['outcome', 'report', 'reportedOutcome', 'reportedReport'])
    requireValue(value[key] === undefined || typeof value[key] === 'string');
  requireValue(
    value.durationMillis === undefined ||
      (integer(value.durationMillis) &&
        value.durationMillis >= 0 &&
        value.durationMillis <= 9223372036854)
  );
  return value as unknown as ComparedStep;
}

export function parsePassComparison(value: unknown): PassComparison {
  requireValue(record(value) && value.version === 1 && value.alignment === 'visit-position');
  const reference = ref(value.reference),
    candidate = ref(value.candidate);
  for (const key of ['sameGoal', 'sameSource', 'sameDefinition'])
    requireValue(typeof value[key] === 'boolean');
  const steps = count(value.stepCount),
    children = count(value.childTaskCount);
  requireValue(
    steps.reference > 0 && steps.candidate > 0 && Math.max(steps.reference, steps.candidate) <= 4096
  );
  requireValue(
    Array.isArray(value.steps) && value.steps.length === Math.max(steps.reference, steps.candidate)
  );
  let previousReference = reference.pass - 1,
    previousCandidate = candidate.pass - 1;
  let referenceChildren = 0,
    candidateChildren = 0;
  value.steps.forEach((row, index) => {
    requireValue(record(row) && row.position === index + 1);
    requireValue(
      (row.reference !== undefined) === index < steps.reference &&
        (row.candidate !== undefined) === index < steps.candidate
    );
    const left = row.reference === undefined ? undefined : step(row.reference, previousReference);
    const right = row.candidate === undefined ? undefined : step(row.candidate, previousCandidate);
    if (left) {
      requireValue(index !== 0 || left.sequence === reference.pass);
      previousReference = left.sequence;
      referenceChildren += left.childTasks;
    }
    if (right) {
      requireValue(index !== 0 || right.sequence === candidate.pass);
      previousCandidate = right.sequence;
      candidateChildren += right.childTasks;
    }
    const expectedNode =
      !left || !right ? 'missing' : left.nodeId === right.nodeId ? 'same' : 'different';
    requireValue(row.node === expectedNode);
    for (const key of ['outcome', 'report', 'reportedOutcome', 'reportedReport']) {
      requireValue(
        row.node === 'same'
          ? ['same', 'different', 'unknown'].includes(String(row[key]))
          : row[key] === 'not_comparable'
      );
    }
    if (
      row.node === 'same' &&
      left?.durationMillis !== undefined &&
      right?.durationMillis !== undefined
    )
      requireValue(row.durationDeltaMillis === right.durationMillis - left.durationMillis);
    else requireValue(row.durationDeltaMillis === undefined);
  });
  requireValue(
    referenceChildren === children.reference && candidateChildren === children.candidate
  );
  return value as unknown as PassComparison;
}

function comparisonPath(
  reference: string,
  referencePass: number,
  candidate: string,
  candidatePass: number
): string {
  if (
    !reference ||
    reference.length > 256 ||
    !candidate ||
    candidate.length > 256 ||
    !positive(referencePass) ||
    !positive(candidatePass)
  )
    throw new Error('Bounded swarm IDs and positive pass numbers are required');
  const query = new URLSearchParams({ swarm: candidate, pass: String(candidatePass) });
  return `/api/swarms/${encodeURIComponent(reference)}/passes/${referencePass}/compare?${query}`;
}
export const passComparisonApi = {
  get: async (
    reference: string,
    referencePass: number,
    candidate: string,
    candidatePass: number
  ): Promise<PassComparison> => {
    const result = parsePassComparison(
      await request<unknown>(
        'GET',
        comparisonPath(reference, referencePass, candidate, candidatePass)
      )
    );
    requireValue(
      result.reference.swarmId === reference &&
        result.reference.pass === referencePass &&
        result.candidate.swarmId === candidate &&
        result.candidate.pass === candidatePass
    );
    return result;
  },
  downloadUrl: (
    reference: string,
    referencePass: number,
    candidate: string,
    candidatePass: number
  ): string => `${comparisonPath(reference, referencePass, candidate, candidatePass)}&download=1`,
};
