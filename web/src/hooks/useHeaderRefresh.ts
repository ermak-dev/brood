import { useEffect } from 'react';

export type HeaderRefreshHandler = () => void | Promise<void>;

/** Один проект может содержать несколько независимых таблиц роёв. Каждая
 * подписывается на обновление и снимает только собственную подписку. */
export function createRefreshRegistry() {
  const handlers = new Map<symbol, HeaderRefreshHandler>();
  return {
    subscribe(handler: HeaderRefreshHandler) {
      const key = Symbol();
      handlers.set(key, handler);
      return () => {
        handlers.delete(key);
      };
    },
    async refresh() {
      await Promise.all([...new Set(handlers.values())].map(async (handler) => handler()));
    },
  };
}

const registry = createRefreshRegistry();
export const triggerHeaderRefresh = () => registry.refresh();

export const useHeaderRefresh = (handler?: HeaderRefreshHandler | null, enabled = true) => {
  useEffect(() => {
    if (enabled && handler) return registry.subscribe(handler);
  }, [enabled, handler]);
};
