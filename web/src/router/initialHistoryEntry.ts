// Запись, созданная мимо роутера (адресная строка, location.hash), не имеет
// idx роутера; если её state не удалось записать, роутер угадывает индексы
// следующих записей, и разница индексов перестаёт быть расстоянием.
let historyWritesDenied = false;

function identifyHistoryEntry(): void {
  try {
    const state: unknown = window.history.state;
    if (state !== null && (typeof state !== 'object' || Array.isArray(state))) return;
    const existing = state as Record<string, unknown> | null;
    if (existing?.key && existing.key !== 'default') return;
    const key = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}-${Math.random().toString(36).slice(2)}`;
    window.history.replaceState({ ...existing, key }, '');
  } catch {
    // History writes may be denied; unidentified entries cannot be restored reliably.
    historyWritesDenied = true;
  }
}

export function identifyInitialHistoryEntry(): void {
  identifyHistoryEntry();
  window.addEventListener('popstate', identifyHistoryEntry);
}

/** Индекс записи роутера; у внешней записи его нет, у её потомков он NaN. */
export function readHistoryIndex(state: unknown): number | undefined {
  if (!state || typeof state !== 'object') return undefined;
  const idx: unknown = (state as Record<string, unknown>).idx;
  return typeof idx === 'number' && Number.isInteger(idx) ? idx : undefined;
}

export function historyStateWritesDenied(): boolean {
  return historyWritesDenied;
}

/**
 * Роутер отменяет Back/Forward, возвращаясь на разницу idx: без индекса он
 * не отменяет ничего, NaN и 0 дают history.go(0) — перезагрузку, а угаданный
 * индекс уводит на чужую запись. Отмена безопасна только между записями роутера.
 */
export function canCancelTraversal(
  fromIndex: number | undefined,
  toIndex: number | undefined,
  writesDenied: boolean
): boolean {
  return fromIndex !== undefined && toIndex !== undefined && fromIndex !== toIndex && !writesDenied;
}
