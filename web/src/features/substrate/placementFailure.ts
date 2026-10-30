import { BroodApiError } from '@/services/brood/http';

export interface PlacementReason {
  code: string;
  state?: string;
  required?: string;
  available?: string;
}

export interface PlacementDiagnostic {
  workers: { name: string; reasons: PlacementReason[] }[];
  templateSelector: Record<string, string>;
  actorSelector: Record<string, string>;
}

const record = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

function selector(value: unknown): Record<string, string> | null {
  if (value == null) return {};
  if (!record(value) || !Object.values(value).every((item) => typeof item === 'string')) return null;
  return value as Record<string, string>;
}

function reason(value: unknown): value is PlacementReason {
  if (!record(value) || typeof value.code !== 'string') return false;
  if (value.state !== undefined && typeof value.state !== 'string') return false;
  return ['required', 'available'].every((key) => value[key] === undefined ||
    (typeof value[key] === 'string' && /^-?\d+$/.test(value[key])));
}

function diagnostic(value: unknown): PlacementDiagnostic | null {
  if (!record(value) || !Array.isArray(value.workers)) return null;
  const templateSelector = selector(value.templateSelector);
  const actorSelector = selector(value.actorSelector);
  if (!templateSelector || !actorSelector) return null;
  const workers: PlacementDiagnostic['workers'] = [];
  for (const worker of value.workers) {
    if (!record(worker) || typeof worker.name !== 'string' ||
        !Array.isArray(worker.reasons) || !worker.reasons.every(reason)) return null;
    workers.push({ name: worker.name, reasons: worker.reasons });
  }
  return { workers, templateSelector, actorSelector };
}

// Старые и повреждённые ответы не становятся выдуманными причинами отказа.
// Отсутствие подробностей нормально для участника без доступа к инфраструктуре.
export function placementFailure(error: unknown): { diagnostic: PlacementDiagnostic | null } | null {
  if (!(error instanceof BroodApiError) || !record(error.body) ||
      error.body.code !== 'actor_placement_unavailable') return null;
  return { diagnostic: diagnostic(error.body.placement) };
}
