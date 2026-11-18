import { request } from './http';
import { parseChildGroup, type ChildGroup, type ExecutorFailure } from './children';
import { parsePolicyCandidate, type PolicyCandidate } from './policyEvidence';
import { parseSwarmRequest, type SwarmRequest } from './requests';

export interface TraceAttempt {
  number: number;
  mode: 'send' | 'probe';
  createdAt: string;
  deadlineAt: string;
  resultRetained: boolean;
  outcome?: string;
  detail?: string;
  observedAt?: string;
}

export interface TraceDelivery {
  id: string;
  sourceKind: 'step' | 'answer' | 'child';
  sourceId: string;
  actorUid: string;
  actorIncarnation: number;
  createdAt: string;
  probeOnly: boolean;
  frame: string;
  attempts: TraceAttempt[];
}

export interface TraceEphemeralJob {
  id: string;
  requestId: string;
  task: string;
  poolUid: string;
  queuedAt: string;
  source: { origin: string; commit: string };
  allocation?: { actorUid: string; actorIncarnation: number; allocatedAt: string };
  createAuthorization?: {
    state: 'queued' | 'reserved' | 'legacy';
    createdAt: string;
    reservedAt?: string;
  };
  attestation?: {
    state: 'pending' | 'unconfirmed' | 'attested';
    verifiedOrigin?: string;
    verifiedCommit?: string;
    createdAt: string;
    updatedAt: string;
  };
  childId?: string;
  inventory?: {
    uid: string;
    incarnation: number;
    currentName?: string;
    phase: string;
    desired: string;
    operation: string;
    stage: string;
    updatedAt: string;
  };
  inventoryUnavailable?: 'not_retained' | 'incarnation_changed' | 'scope_changed';
  resultRetention?: {
    childId: string;
    childRevision: number;
    outcome: string;
    parentOutcome: string;
    retainedAt: string;
  };
  failure?: { reason: string; failedAt: string };
  executorFailure?: ExecutorFailure;
}

export interface TraceStep {
  sequence: number;
  nodeId: string;
  state: string;
  startedAt?: string;
  finishedAt?: string;
  requestId?: string;
  actor?: { uid: string; incarnation: number; currentName?: string };
  attestedCommit?: string;
  outcome?: string;
  report?: string;
  detail?: string;
  instruction?: TraceDelivery;
  childInstructions: TraceDelivery[];
  ephemeralJobs: TraceEphemeralJob[];
  children?: ChildGroup;
  reportedResult?: {
    requestId: string;
    actorUid: string;
    actorIncarnation: number;
    outcome: string;
    report: string;
    createdAt: string;
  };
  resultObservation?: { id: string; state: string; detail?: string; revision: number };
  artifactEvidence?: {
    commit: string;
    baseCommit: string;
    producerUid: string;
    incarnation: number;
    paths: string[];
  };
  policyCandidates: PolicyCandidate[];
}

export interface TraceRequest extends SwarmRequest {
  actorUid?: string;
  actorIncarnation?: number;
  answeredByUid?: string;
  createdDuring?: { pass: number; observedSequence: number };
  deliveryHistory?: TraceDelivery;
}

export interface PassTrace {
  version: 1;
  swarmId: string;
  pass: number;
  goal: string;
  source: { origin: string; commit: string };
  definition: { name: string; version: number; revision: string };
  graph: string;
  current: boolean;
  execution?: string;
  steps: TraceStep[];
  requests: TraceRequest[];
  unscopedActorRequests: TraceRequest[];
  gaps: Array<{ kind: string; sequence?: number; requestId?: string }>;
}

const INVALID = 'Invalid pass trace response';
const record = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value);
const positive = (value: unknown): value is number =>
  typeof value === 'number' && Number.isSafeInteger(value) && value > 0;
const text = (value: unknown): value is string => typeof value === 'string' && value.length > 0;
const strings = (value: unknown): value is string[] =>
  Array.isArray(value) && value.every((item) => typeof item === 'string');
function requireValue(condition: unknown): asserts condition {
  if (!condition) throw new Error(INVALID);
}
function optionalText(value: Record<string, unknown>, keys: string[]) {
  for (const key of keys) requireValue(value[key] === undefined || typeof value[key] === 'string');
}

function parseDelivery(value: unknown): TraceDelivery {
  requireValue(record(value));
  requireValue(
    ['id', 'sourceId', 'actorUid', 'createdAt', 'frame'].every((key) => text(value[key]))
  );
  requireValue(
    value.sourceKind === 'step' || value.sourceKind === 'answer' || value.sourceKind === 'child'
  );
  requireValue(positive(value.actorIncarnation) && typeof value.probeOnly === 'boolean');
  requireValue(Array.isArray(value.attempts) && value.attempts.length <= 8);
  value.attempts.forEach((attempt, index) => {
    requireValue(record(attempt) && attempt.number === index + 1);
    requireValue(attempt.mode === 'send' || attempt.mode === 'probe');
    requireValue(text(attempt.createdAt) && text(attempt.deadlineAt));
    requireValue(typeof attempt.resultRetained === 'boolean');
    optionalText(attempt, ['outcome', 'detail', 'observedAt']);
    if (attempt.resultRetained) {
      requireValue(
        ['delivered', 'duplicate', 'not_sent', 'unconfirmed'].includes(String(attempt.outcome))
      );
      if (attempt.mode === 'probe')
        requireValue(attempt.outcome === 'duplicate' || attempt.outcome === 'unconfirmed');
    } else {
      requireValue(attempt.outcome === undefined && attempt.observedAt === undefined);
    }
  });
  return value as unknown as TraceDelivery;
}

function parseEphemeralJob(value: unknown): TraceEphemeralJob {
  requireValue(record(value));
  requireValue(['id', 'requestId', 'task', 'poolUid', 'queuedAt'].every((key) => text(value[key])));
  requireValue(record(value.source) && text(value.source.origin) && text(value.source.commit));
  optionalText(value, ['childId']);
  const allocation = value.allocation;
  if (allocation !== undefined) {
    requireValue(record(allocation));
    requireValue(text(allocation.actorUid) && positive(allocation.actorIncarnation));
    requireValue(text(allocation.allocatedAt));
  }
  if (value.createAuthorization !== undefined) {
    const grant = value.createAuthorization;
    requireValue(record(grant) && text(grant.createdAt));
    requireValue(text(grant.state) && ['queued', 'reserved', 'legacy'].includes(grant.state));
    requireValue(
      grant.state === 'reserved' ? text(grant.reservedAt) : grant.reservedAt === undefined
    );
  }
  if (value.attestation !== undefined) {
    const proof = value.attestation;
    requireValue(record(proof) && text(proof.createdAt) && text(proof.updatedAt));
    requireValue(text(proof.state) && ['pending', 'unconfirmed', 'attested'].includes(proof.state));
    requireValue(
      proof.state === 'attested'
        ? proof.verifiedOrigin === value.source.origin &&
            proof.verifiedCommit === value.source.commit
        : proof.verifiedOrigin === undefined && proof.verifiedCommit === undefined
    );
  }
  if (value.inventory !== undefined) {
    const inventory = value.inventory;
    requireValue(record(inventory) && record(allocation));
    requireValue(
      inventory.uid === allocation.actorUid && inventory.incarnation === allocation.actorIncarnation
    );
    requireValue(['phase', 'desired', 'updatedAt'].every((key) => text(inventory[key])));
    requireValue(typeof inventory.operation === 'string' && typeof inventory.stage === 'string');
    optionalText(inventory, ['currentName']);
    requireValue(value.inventoryUnavailable === undefined);
  }
  if (value.inventoryUnavailable !== undefined) {
    requireValue(
      text(value.inventoryUnavailable) &&
        ['not_retained', 'incarnation_changed', 'scope_changed'].includes(
          value.inventoryUnavailable
        )
    );
  }
  if (value.resultRetention !== undefined) {
    const result = value.resultRetention;
    requireValue(record(result) && positive(result.childRevision));
    requireValue(
      ['childId', 'outcome', 'parentOutcome', 'retainedAt'].every((key) => text(result[key]))
    );
    requireValue(result.childId === value.childId);
  }
  if (value.failure !== undefined) {
    const failure = value.failure;
    requireValue(record(failure) && text(failure.reason) && text(failure.failedAt));
    requireValue(!/^\p{White_Space}*$/u.test(failure.reason));
    requireValue(value.childId === undefined && value.resultRetention === undefined);
  }
  if (value.executorFailure !== undefined) {
    const fact = value.executorFailure;
    requireValue(record(fact));
    requireValue(
      ['intentId', 'childId', 'requestId', 'reason', 'failedAt'].every((key) => text(fact[key]))
    );
    requireValue(text(fact.reason) && !/^\p{White_Space}*$/u.test(fact.reason));
    requireValue(
      fact.intentId === value.id &&
        fact.childId === value.childId &&
        fact.requestId === value.requestId
    );
    requireValue(value.failure === undefined && value.resultRetention === undefined);
  }
  return value as unknown as TraceEphemeralJob;
}

function parseStep(value: unknown, swarm: string, pass: number): TraceStep {
  requireValue(record(value));
  requireValue(positive(value.sequence) && value.sequence >= pass);
  requireValue(text(value.nodeId) && text(value.state) && Array.isArray(value.policyCandidates));
  optionalText(value, [
    'startedAt',
    'finishedAt',
    'requestId',
    'attestedCommit',
    'outcome',
    'report',
    'detail',
  ]);
  if (value.actor !== undefined) {
    requireValue(record(value.actor) && text(value.actor.uid) && positive(value.actor.incarnation));
    optionalText(value.actor, ['currentName']);
  }
  if (value.reportedResult !== undefined) {
    const reported = value.reportedResult;
    requireValue(record(reported));
    requireValue(
      ['requestId', 'actorUid', 'outcome', 'createdAt'].every((key) => text(reported[key]))
    );
    requireValue(positive(reported.actorIncarnation) && typeof reported.report === 'string');
    requireValue(reported.requestId === value.requestId);
    requireValue(
      record(value.actor) &&
        reported.actorUid === value.actor.uid &&
        reported.actorIncarnation === value.actor.incarnation
    );
  }
  if (value.resultObservation !== undefined) {
    const observed = value.resultObservation;
    requireValue(
      record(observed) && text(observed.id) && text(observed.state) && positive(observed.revision)
    );
    optionalText(observed, ['detail']);
  }
  if (value.artifactEvidence !== undefined) {
    const evidence = value.artifactEvidence;
    requireValue(record(evidence));
    requireValue(text(evidence.commit) && text(evidence.baseCommit) && text(evidence.producerUid));
    requireValue(positive(evidence.incarnation) && strings(evidence.paths));
    requireValue(
      record(value.actor) &&
        evidence.producerUid === value.actor.uid &&
        evidence.incarnation === value.actor.incarnation
    );
  }
  if (value.instruction !== undefined) {
    const delivery = parseDelivery(value.instruction);
    requireValue(delivery.sourceKind === 'step' && delivery.sourceId === String(value.sequence));
    if (record(value.actor))
      requireValue(
        delivery.actorUid === value.actor.uid &&
          delivery.actorIncarnation === value.actor.incarnation
      );
  }
  let children: ChildGroup | undefined;
  if (value.children !== undefined) {
    children = parseChildGroup(value.children);
    requireValue(children.pass === pass && children.parentSequence === value.sequence);
    value = { ...value, children };
  }
  requireValue(record(value));
  for (const candidate of value.policyCandidates as unknown[]) {
    const parsed = parsePolicyCandidate(candidate);
    requireValue(
      parsed.swarmId === swarm && parsed.pass === pass && parsed.sequence === value.sequence
    );
  }
  const childInstructions = value.childInstructions === undefined ? [] : value.childInstructions;
  requireValue(Array.isArray(childInstructions));
  const deliveries = childInstructions.map((item) => {
    const delivery = parseDelivery(item);
    requireValue(delivery.sourceKind === 'child');
    return delivery;
  });
  const ephemeralJobs = value.ephemeralJobs === undefined ? [] : value.ephemeralJobs;
  requireValue(Array.isArray(ephemeralJobs));
  const jobs = ephemeralJobs.map(parseEphemeralJob);
  const failures = jobs.flatMap((job) => (job.executorFailure ? [job.executorFailure] : []));
  requireValue(new Set(failures.map((fact) => fact.intentId)).size === failures.length);
  requireValue(new Set(failures.map((fact) => fact.childId)).size === failures.length);
  if (children && value.ephemeralJobs !== undefined) {
    const childFailures = children.children.flatMap((child) =>
      child.executorFailure ? [child.executorFailure] : []
    );
    requireValue(childFailures.length === failures.length);
    for (const fact of failures) {
      const member = childFailures.find((childFact) => childFact.childId === fact.childId);
      requireValue(
        member !== undefined &&
          (['intentId', 'childId', 'requestId', 'reason', 'failedAt'] as const).every(
            (key) => member[key] === fact[key]
          )
      );
    }
  }
  return { ...value, childInstructions: deliveries, ephemeralJobs: jobs } as unknown as TraceStep;
}

function parseRequest(
  value: unknown,
  swarm: string,
  pass: number,
  sequences: Set<number>,
  unscoped: boolean
): TraceRequest {
  requireValue(record(value));
  const base = parseSwarmRequest(value);
  requireValue(base.swarmId === swarm);
  optionalText(value, ['actorUid', 'answeredByUid']);
  if (value.actorIncarnation !== undefined) requireValue(positive(value.actorIncarnation));
  if (base.origin === 'actor') {
    requireValue(
      base.sequence === undefined && text(value.actorUid) && positive(value.actorIncarnation)
    );
    if (unscoped) {
      requireValue(value.createdDuring === undefined);
    } else {
      requireValue(record(value.createdDuring) && value.createdDuring.pass === pass);
      requireValue(
        positive(value.createdDuring.observedSequence) &&
          sequences.has(value.createdDuring.observedSequence)
      );
    }
  } else {
    requireValue(!unscoped && positive(base.sequence) && sequences.has(base.sequence));
  }
  if (value.deliveryHistory !== undefined) {
    const delivery = parseDelivery(value.deliveryHistory);
    requireValue(delivery.sourceKind === 'answer' && delivery.sourceId === base.id);
    requireValue(
      delivery.actorUid === value.actorUid && delivery.actorIncarnation === value.actorIncarnation
    );
  }
  return { ...value, ...base } as unknown as TraceRequest;
}

export function parsePassTrace(value: unknown): PassTrace {
  requireValue(record(value) && value.version === 1);
  requireValue(
    text(value.swarmId) && positive(value.pass) && text(value.goal) && text(value.graph)
  );
  requireValue(record(value.source) && text(value.source.origin) && text(value.source.commit));
  requireValue(
    record(value.definition) &&
      text(value.definition.name) &&
      positive(value.definition.version) &&
      text(value.definition.revision)
  );
  requireValue(typeof value.current === 'boolean');
  requireValue(value.current ? text(value.execution) : value.execution === undefined);
  requireValue(Array.isArray(value.steps) && value.steps.length > 0);
  requireValue(
    Array.isArray(value.requests) &&
      Array.isArray(value.unscopedActorRequests) &&
      Array.isArray(value.gaps)
  );
  requireValue(
    value.steps.length + value.requests.length + value.unscopedActorRequests.length <= 4096
  );
  const swarm = value.swarmId;
  const pass = value.pass;
  const steps = value.steps.map((step) => parseStep(step, swarm, pass));
  requireValue(steps[0].sequence === pass);
  requireValue(
    steps.every((step, index) => index === 0 || step.sequence > steps[index - 1].sequence)
  );
  const sequences = new Set(steps.map((step) => step.sequence));
  const requests = value.requests.map((item) => parseRequest(item, swarm, pass, sequences, false));
  const unscopedActorRequests = value.unscopedActorRequests.map((item) =>
    parseRequest(item, swarm, pass, sequences, true)
  );
  const ids = [...requests, ...unscopedActorRequests].map((item) => item.id);
  requireValue(new Set(ids).size === ids.length);
  for (const gap of value.gaps) {
    requireValue(record(gap) && text(gap.kind));
    requireValue(gap.sequence === undefined || positive(gap.sequence));
    optionalText(gap, ['requestId']);
  }
  return { ...value, steps, requests, unscopedActorRequests } as unknown as PassTrace;
}

function tracePath(swarm: string, pass: number): string {
  if (!swarm || !positive(pass)) throw new Error('Swarm and positive pass number are required');
  return `/api/swarms/${encodeURIComponent(swarm)}/passes/${pass}`;
}

export const passTraceApi = {
  get: async (swarm: string, pass: number): Promise<PassTrace> => {
    const trace = parsePassTrace(await request<unknown>('GET', tracePath(swarm, pass)));
    requireValue(trace.swarmId === swarm && trace.pass === pass);
    return trace;
  },
  downloadUrl: (swarm: string, pass: number): string => `${tracePath(swarm, pass)}?download=1`,
};
