/**
 * brood: an actor template draft in ateapi protojson -- pure helpers behind the
 * template editor: a blank draft, a copy, what goes to ateapi, and whether a
 * JSON draft has the shape the form needs.
 */

import type { ActorTemplate, Container } from '@/services/brood/substrate';
import { clone } from './format';

export const FIDELITIES = [
  'SNAPSHOT_FIDELITY_VOLUMES',
  'SNAPSHOT_FIDELITY_ROOTFS',
  'SNAPSHOT_FIDELITY_MEMORY',
] as const;

const GIB = String(2 * 1024 * 1024 * 1024);

export function blankTemplate(): ActorTemplate {
  return {
    metadata: { name: '' },
    containers: [{ name: 'main', image: '' }],
    resources: { limits: { cpuMillicores: '1000', memoryBytes: GIB } },
    snapshotConfig: { preferredFidelity: 'SNAPSHOT_FIDELITY_MEMORY' },
  };
}

/** A template to create from another: everything but identity and status. */
export function copyOf(source: ActorTemplate): ActorTemplate {
  const copy = clone(source);
  delete copy.status;
  copy.metadata = { name: `${source.metadata.name}-copy`.slice(0, 63) };
  return copy;
}

const isNamed = (value: unknown) =>
  typeof value === 'object' && value !== null && typeof (value as { name?: unknown }).name === 'string';

const isNamedList = (value: unknown) => value === undefined || (Array.isArray(value) && value.every(isNamed));

/** Why a JSON draft cannot back the form, or null: every field the form and
 * cleaned() read must have its protojson shape, or rendering would throw.
 * ateapi validates the rest on save. */
export function shapeProblem(draft: unknown): string | null {
  const tpl = draft as ActorTemplate;
  if (typeof draft !== 'object' || draft === null || !Array.isArray(tpl.containers)) return 'containers';
  for (const [i, c] of tpl.containers.entries()) {
    if (!isNamed(c) || typeof c.image !== 'string') return `containers[${i}]: name, image`;
    if (!isNamedList(c.env)) return `containers[${i}].env`;
    if (!isNamedList(c.volumeMounts)) return `containers[${i}].volumeMounts`;
    for (const field of ['command', 'args'] as const) {
      const list = c[field];
      if (list !== undefined && !(Array.isArray(list) && list.every((v) => typeof v === 'string'))) {
        return `containers[${i}].${field}`;
      }
    }
  }
  if (!isNamedList(tpl.volumes)) return 'volumes';
  return null;
}

/** What goes to ateapi: draft rows without a name dropped. */
export function cleaned(draft: ActorTemplate): ActorTemplate {
  const out = clone(draft);
  delete out.status;
  out.metadata = { name: draft.metadata.name };
  out.containers = (out.containers ?? []).map((c) => {
    const next: Container = { ...c };
    if (next.env) next.env = next.env.filter((e) => e.name.trim());
    if (next.volumeMounts) next.volumeMounts = next.volumeMounts.filter((m) => m.name.trim());
    return next;
  });
  if (out.volumes) out.volumes = out.volumes.filter((v) => v.name.trim());
  return out;
}

export const asJson = (draft: ActorTemplate) => JSON.stringify(cleaned(draft), null, 2);

export function volumeKind(volume: Record<string, unknown>): string {
  return Object.keys(volume).find((key) => key !== 'name') ?? '';
}

