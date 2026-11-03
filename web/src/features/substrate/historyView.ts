import type { HistoryEntry, HistoryPage } from '@/services/brood/commands';

export interface LoadedHistory {
  entries: HistoryEntry[];
  cursor: string;
  hasMore: boolean;
  partial: boolean;
  /** Transcript сменился во время просмотра: показан заново с начала. */
  restarted: boolean;
}

/** Следующая страница дописывается к прочитанным; первая или страница
 * сменившегося transcript (reset) начинает список заново. */
export function mergeHistory(
  previous: LoadedHistory | null,
  page: HistoryPage,
  first: boolean
): LoadedHistory {
  const fresh = first || page.reset || !previous;
  return {
    entries: fresh ? page.entries : [...previous.entries, ...page.entries],
    cursor: page.cursor,
    hasMore: page.hasMore,
    partial: page.partial || (!fresh && previous.partial),
    restarted: !first && page.reset,
  };
}
