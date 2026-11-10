import type { OAuthModelAliasEntry } from '@/types';
import { normalizeProviderKey } from './constants';

type ModelAliasDraftEntry = {
  id?: string;
  name?: string;
  alias?: string;
  fork?: boolean;
  forceMapping?: boolean;
};

export const getStringSetSignature = (values: Iterable<string>): string =>
  JSON.stringify(
    Array.from(new Set(values), (value) => value.trim())
      .filter(Boolean)
      .sort()
  );

export const getModelAliasDraftSignature = (entries: ModelAliasDraftEntry[]): string =>
  JSON.stringify(
    entries
      .map((entry) => ({
        name: entry.name ?? '',
        alias: entry.alias ?? '',
        fork: entry.fork === true,
        forceMapping: typeof entry.forceMapping === 'boolean' ? entry.forceMapping : undefined,
      }))
      .filter(
        (entry) =>
          entry.name !== '' ||
          entry.alias !== '' ||
          entry.fork !== true ||
          entry.forceMapping !== undefined
      )
  );

export const isOAuthEditorDirty = (
  initialProvider: string,
  currentProvider: string,
  baselineContentSignature: string,
  currentContentSignature: string
): boolean =>
  normalizeProviderKey(initialProvider) !== normalizeProviderKey(currentProvider) ||
  baselineContentSignature !== currentContentSignature;

// Сохранённый черновик редактора (useDraftRecovery) — непроверенные данные хранилища.
const isModelAliasEntry = (value: unknown): value is OAuthModelAliasEntry => {
  if (!value || typeof value !== 'object') return false;
  const { name, alias, fork, forceMapping } = value as Record<string, unknown>;
  return (
    typeof name === 'string' &&
    typeof alias === 'string' &&
    (fork === undefined || typeof fork === 'boolean') &&
    (forceMapping === undefined || typeof forceMapping === 'boolean')
  );
};

export const parseModelAliasDraft = (
  value: unknown,
  baselineSignature: string
): OAuthModelAliasEntry[] | null =>
  Array.isArray(value) &&
  value.every(isModelAliasEntry) &&
  getModelAliasDraftSignature(value) !== baselineSignature
    ? value
    : null;

export const parseExcludedModelsDraft = (
  value: unknown,
  baselineSignature: string
): string[] | null =>
  Array.isArray(value) &&
  value.every((rule): rule is string => typeof rule === 'string') &&
  getStringSetSignature(value) !== baselineSignature
    ? value
    : null;
