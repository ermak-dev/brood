/**
 * brood: pure helpers for Substrate objects in protojson -- enum names, badge
 * tones, sizes, label maps and the k8s short-name rule ateapi enforces.
 */

import { formatDateValue } from '@/utils/format';
import type { Selector } from '@/services/brood/substrate';

export const DASH = '—';

export type Tone = 'success' | 'warning' | 'danger' | 'muted';

/** 'ACTOR_STATE_RUNNING' -> 'running'; the prefix is the enum's name. */
export function shortState(state: string | undefined, prefix: string): string {
  return (state ?? '').replace(prefix, '').toLowerCase();
}

const TONES: Record<string, Tone> = {
  running: 'success',
  active: 'success',
  suspended: 'muted',
  resuming: 'warning',
  suspending: 'warning',
  reverting: 'warning',
  draining: 'warning',
  unavailable: 'warning',
  deleting: 'warning',
  crashed: 'danger',
};

export function toneOf(state: string): Tone {
  return TONES[state] ?? 'muted';
}

export function formatDate(value: string | undefined, language: string): string {
  return value ? formatDateValue(value, language) : DASH;
}

/** int64 millicores (string in protojson) -> '1.5 CPU' / '250m'; the number
 * and its unit never part across lines. */
export function formatCpu(millicores: string | number | undefined): string {
  const value = Number(millicores ?? 0);
  if (!value) return DASH;
  return value >= 1000 ? `${+(value / 1000).toFixed(2)}\u00a0CPU` : `${value}m`;
}

const MIB = 1024 * 1024;

/** int64 bytes (string in protojson) -> '512 MiB' / '2 GiB'. */
export function formatMemory(bytes: string | number | undefined): string {
  const value = Number(bytes ?? 0);
  if (!value) return DASH;
  return value >= 1024 * MIB
    ? `${+(value / 1024 / MIB).toFixed(2)}\u00a0GiB`
    : `${+(value / MIB).toFixed(1)}\u00a0MiB`;
}

export const bytesToMib = (bytes: string | undefined) =>
  bytes ? String(+(Number(bytes) / MIB).toFixed(2)) : '';

export const mibToBytes = (mib: string) =>
  mib.trim() ? String(Math.round(Number(mib) * MIB)) : undefined;

export interface Pair {
  key: string;
  value: string;
}

export const toPairs = (map: Record<string, string> | undefined): Pair[] =>
  Object.entries(map ?? {}).map(([key, value]) => ({ key, value }));

/** Pairs -> map; rows without a key are drafts and are dropped. */
export function fromPairs(pairs: Pair[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const { key, value } of pairs) {
    if (key.trim()) out[key.trim()] = value;
  }
  return out;
}

export function selectorText(selector: Selector | undefined): string {
  const labels = Object.entries(selector?.matchLabels ?? {});
  return labels.length ? labels.map(([k, v]) => `${k}=${v}`).join(', ') : DASH;
}

/** k8s short name, as ateapi validates names: lowercase alphanumerics and '-',
 * starting and ending alphanumeric, at most 63 characters. */
export function isShortName(name: string): boolean {
  return name.length <= 63 && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name);
}

export const clone = <T,>(value: T): T => JSON.parse(JSON.stringify(value)) as T;
