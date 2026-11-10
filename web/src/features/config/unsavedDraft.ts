// Черновик config.yaml, сохранённый при уходе со страницы без подтверждения.
// В нём ключи доступа, поэтому он живёт только в памяти вкладки (persistence 'memory').
import { rebaseConfigDraft } from '@/services/api/configPatch';
import type { ConfigEditorMode } from './constants';

export type ConfigDraft = {
  /** Серверный YAML, от которого сделаны правки. */
  base: string;
  /** Полный YAML с правками. */
  yaml: string;
  mode: ConfigEditorMode;
};

export function parseConfigDraft(value: unknown, latestYaml: string): ConfigDraft | null {
  if (!value || typeof value !== 'object') return null;
  const { base, yaml, mode } = value as Record<string, unknown>;
  if (typeof base !== 'string' || typeof yaml !== 'string') return null;
  if (mode !== 'source' && mode !== 'visual') return null;
  return yaml === latestYaml ? null : { base, yaml, mode };
}

/**
 * Правки переносятся на свежий YAML сервера; при конфликте списков черновик
 * восстанавливается как источник, и diff перед сохранением покажет всё.
 */
export function resolveConfigDraft(
  draft: ConfigDraft,
  latestYaml: string
): { yaml: string; mode: ConfigEditorMode } {
  if (draft.base === latestYaml) return { yaml: draft.yaml, mode: draft.mode };
  try {
    return { yaml: rebaseConfigDraft(draft.base, draft.yaml, latestYaml), mode: draft.mode };
  } catch {
    return { yaml: draft.yaml, mode: 'source' };
  }
}
