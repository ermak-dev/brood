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
  }
}

export function identifyInitialHistoryEntry(): void {
  identifyHistoryEntry();
  window.addEventListener('popstate', identifyHistoryEntry);
}
