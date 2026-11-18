import { BroodApiError, request } from './http';
import type { SwarmExecutionResult } from './execution';
import type { SnapshotIdentity } from './pipelines';

export interface EvalSelection {
  referenceSwarmId: string;
  referencePass: number;
  definition: string;
  model: string;
}
export interface SwarmEval {
  swarmId: string;
  referenceSwarmId: string;
  referencePass: number;
  definition: SnapshotIdentity;
  model: string;
  source: { origin: string; commit: string };
  goal: string;
  nonProduction: true;
  execution: string;
  replay: null | {
    requestId: string;
    state: 'applied' | 'stopped';
    reason: string;
    interruptedChildren: boolean;
  };
}

const record = (v: unknown): v is Record<string, unknown> =>
  v !== null && typeof v === 'object' && !Array.isArray(v);
const text = (v: unknown): v is string => typeof v === 'string' && v.length > 0;
const positive = (v: unknown): v is number => Number.isSafeInteger(v) && Number(v) > 0;
const execution = (v: unknown) =>
  typeof v === 'string' &&
  ['idle', 'running', 'waiting', 'succeeded', 'failed', 'stopped'].includes(v);
function requireValue(condition: unknown): asserts condition {
  if (!condition) throw new Error('Invalid swarm eval response');
}

/** Имена моделей проходят ровно ASCII-правило сервера, без trim и дефолта. */
export function validEvalSelection(value: EvalSelection): boolean {
  return (
    text(value.referenceSwarmId) &&
    value.referenceSwarmId.length <= 256 &&
    positive(value.referencePass) &&
    text(value.definition) &&
    text(value.model) &&
    /^[\x21-\x7e]{1,256}$/.test(value.model)
  );
}

/** Строки задачи и причины не нормализуются: FEFF и ZWSP — данные, не пробелы Go. */
export function parseSwarmEval(value: unknown): SwarmEval {
  requireValue(
    record(value) &&
      text(value.swarmId) &&
      text(value.referenceSwarmId) &&
      positive(value.referencePass) &&
      value.nonProduction === true &&
      execution(value.execution)
  );
  requireValue(
    record(value.definition) &&
      text(value.definition.name) &&
      positive(value.definition.version) &&
      text(value.definition.revision)
  );
  requireValue(record(value.source) && text(value.source.origin) && text(value.source.commit));
  requireValue(
    typeof value.goal === 'string' && text(value.model) && /^[\x21-\x7e]{1,256}$/.test(value.model)
  );
  if (value.replay !== null) {
    requireValue(
      record(value.replay) &&
        text(value.replay.requestId) &&
        (value.replay.state === 'applied' || value.replay.state === 'stopped') &&
        typeof value.replay.reason === 'string' &&
        typeof value.replay.interruptedChildren === 'boolean'
    );
  }
  return value as unknown as SwarmEval;
}

function path(id: string): string {
  if (!id || id.length > 256) throw new Error('A bounded swarm ID is required');
  return `/api/swarms/${encodeURIComponent(id)}/eval`;
}
function scoped(value: unknown, id: string): SwarmEval {
  const result = parseSwarmEval(value);
  requireValue(result.swarmId === id);
  return result;
}

export const evalApi = {
  get: async (id: string): Promise<SwarmEval | null> => {
    try {
      return scoped(await request<unknown>('GET', path(id)), id);
    } catch (failure) {
      if (failure instanceof BroodApiError && failure.status === 404) return null;
      throw failure;
    }
  },
  bind: async (id: string, selection: EvalSelection): Promise<SwarmEval> => {
    if (!validEvalSelection(selection)) throw new Error('Invalid eval selection');
    const { referenceSwarmId, referencePass, definition, model } = selection;
    const result = scoped(
      await request<unknown>('POST', path(id), {
        referenceSwarmId,
        referencePass,
        definition,
        model,
      }),
      id
    );
    requireValue(
      result.referenceSwarmId === referenceSwarmId &&
        result.referencePass === referencePass &&
        result.definition.name === definition &&
        result.model === model
    );
    return result;
  },
  start: async (id: string): Promise<SwarmExecutionResult> => {
    const value = await request<unknown>('POST', `${path(id)}/start`);
    requireValue(record(value) && value.swarmId === id && execution(value.execution));
    if (value.definition !== undefined && value.definition !== null) {
      requireValue(
        record(value.definition) &&
          text(value.definition.name) &&
          positive(value.definition.version) &&
          text(value.definition.revision)
      );
    }
    if (value.source !== undefined && value.source !== null) {
      requireValue(record(value.source) && text(value.source.origin) && text(value.source.commit));
    }
    requireValue(value.goal === undefined || typeof value.goal === 'string');
    if (value.step !== undefined && value.step !== null) {
      requireValue(
        record(value.step) &&
          positive(value.step.sequence) &&
          text(value.step.nodeId) &&
          text(value.step.state)
      );
    }
    return value as unknown as SwarmExecutionResult;
  },
};
