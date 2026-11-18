import { request } from './http';

export interface ChildGroupSummary {
  parentSequence: number;
  pass: number;
  parentUid: string;
  parentIncarnation: number;
  mode: string;
  total: number;
  quorum: number;
  state: string;
  revision: number;
  interrupted: boolean;
  registered: number;
  open: number;
  started: number;
}

export interface ChildTask {
  id: string;
  requestId: string;
  role: string;
  executorUid: string;
  executorIncarnation: number;
  state: string;
  delivery: string;
  deliveryDetail: string;
  task: string;
  outcome: string;
  report: string;
  revision: number;
  createdAt: string;
  finishedAt: string;
}

export interface ChildGroup extends ChildGroupSummary {
  readiness: string;
  abortedBy: string;
  abortedAt: string;
  children: ChildTask[];
}

export interface ChildGroupPage {
  items: ChildGroupSummary[];
  nextBefore: number;
}

const INVALID = 'Invalid child history response';
const MAX_CHILDREN = 64;
const MAX_PAGE = 100;
const isRecord = (value: unknown): value is Record<string, unknown> =>
  Boolean(value) && typeof value === 'object' && !Array.isArray(value);

function integer(
  item: Record<string, unknown>,
  key: string,
  min = 0,
  max = Number.MAX_SAFE_INTEGER
) {
  const value = item[key];
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < min || value > max) {
    throw new Error(INVALID);
  }
  return value;
}

function text(item: Record<string, unknown>, key: string, empty = false) {
  const value = item[key];
  if (typeof value !== 'string' || (!empty && !value)) throw new Error(INVALID);
  return value;
}

function optionalText(item: Record<string, unknown>, key: string) {
  return item[key] === undefined ? '' : text(item, key, true);
}

function parseSummary(value: unknown): ChildGroupSummary {
  if (!isRecord(value) || typeof value.interrupted !== 'boolean') throw new Error(INVALID);
  const parentSequence = integer(value, 'parentSequence', 1);
  const total = integer(value, 'total', 1, MAX_CHILDREN);
  const registered = integer(value, 'registered', 0, total);
  return {
    parentSequence,
    pass: integer(value, 'pass', 1, parentSequence),
    parentUid: text(value, 'parentUid'),
    parentIncarnation: integer(value, 'parentIncarnation', 1),
    mode: text(value, 'mode'),
    total,
    quorum: integer(value, 'quorum', 0, total),
    state: text(value, 'state'),
    revision: integer(value, 'revision', 1),
    interrupted: value.interrupted,
    registered,
    open: integer(value, 'open', 0, total),
    started: integer(value, 'started', 0, registered),
  };
}

function parseTask(value: unknown): ChildTask {
  if (!isRecord(value)) throw new Error(INVALID);
  return {
    id: text(value, 'id'),
    requestId: text(value, 'requestId'),
    role: text(value, 'role', true),
    executorUid: text(value, 'executorUid'),
    executorIncarnation: integer(value, 'executorIncarnation', 1),
    state: text(value, 'state'),
    delivery: text(value, 'delivery', true),
    deliveryDetail: optionalText(value, 'deliveryDetail'),
    task: text(value, 'task'),
    outcome: optionalText(value, 'outcome'),
    report: optionalText(value, 'report'),
    revision: integer(value, 'revision', 1),
    createdAt: text(value, 'createdAt'),
    finishedAt: optionalText(value, 'finishedAt'),
  };
}

export function parseChildGroup(value: unknown): ChildGroup {
  const summary = parseSummary(value);
  if (!isRecord(value) || !Array.isArray(value.children) || value.children.length > MAX_CHILDREN) {
    throw new Error(INVALID);
  }
  const children = value.children.map(parseTask);
  if (
    children.length !== summary.registered ||
    new Set(children.map((task) => task.id)).size !== children.length
  ) {
    throw new Error(INVALID);
  }
  const started = children.filter(
    (task) => task.state === 'running' || task.state === 'completed'
  ).length;
  const unsettled = children.filter(
    (task) => task.state === 'pending' || task.state === 'running'
  ).length;
  const open = summary.state === 'aborted' ? 0 : summary.total - children.length + unsettled;
  if (summary.started !== started || summary.open !== open) throw new Error(INVALID);
  return {
    ...summary,
    readiness: text(value, 'readiness'),
    abortedBy: optionalText(value, 'abortedBy'),
    abortedAt: optionalText(value, 'abortedAt'),
    children,
  };
}

export function parseChildGroupPage(value: unknown): ChildGroupPage {
  if (!isRecord(value) || !Array.isArray(value.items) || value.items.length > MAX_PAGE) {
    throw new Error(INVALID);
  }
  const items = value.items.map(parseSummary);
  const nextBefore = integer(value, 'nextBefore');
  if (
    items.some((item, index) => index > 0 && item.parentSequence >= items[index - 1].parentSequence)
  ) {
    throw new Error(INVALID);
  }
  if (nextBefore !== 0 && nextBefore !== items[items.length - 1]?.parentSequence) throw new Error(INVALID);
  return { items, nextBefore };
}

const positive = (value: number) => Number.isSafeInteger(value) && value > 0;
const path = (swarm: string) => {
  if (!swarm) throw new Error('Swarm is required');
  return `/api/swarms/${encodeURIComponent(swarm)}/children`;
};

export const childrenApi = {
  list: async (swarm: string, before = 0, limit = 25): Promise<ChildGroupPage> => {
    if (!Number.isSafeInteger(before) || before < 0 || !positive(limit) || limit > MAX_PAGE) {
      throw new Error('Invalid child history pagination');
    }
    return parseChildGroupPage(
      await request<unknown>('GET', `${path(swarm)}?before=${before}&limit=${limit}`)
    );
  },
  group: async (swarm: string, sequence: number): Promise<ChildGroup> => {
    if (!positive(sequence)) throw new Error('Invalid parent sequence');
    const group = parseChildGroup(await request<unknown>('GET', `${path(swarm)}/${sequence}`));
    if (group.parentSequence !== sequence) throw new Error(INVALID);
    return group;
  },
  abort: async (swarm: string, sequence: number, revision: number) => {
    if (!positive(sequence) || !positive(revision) || revision === Number.MAX_SAFE_INTEGER) {
      throw new Error('Invalid child abort identity');
    }
    const body = await request<unknown>('POST', `${path(swarm)}/${sequence}/abort`, {
      revision,
      confirm: true,
    });
    if (
      !isRecord(body) ||
      body.state !== 'aborted' ||
      body.open !== 0 ||
      body.revision !== revision + 1
    ) {
      throw new Error(INVALID);
    }
    return { state: 'aborted' as const, revision: body.revision as number, open: 0 };
  },
};
