import { useCallback, useEffect, useRef, useState } from 'react';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useVisibleInterval } from '@/hooks/useInterval';
import { errorMessage } from '@/services/brood/http';
import { useAuthStore } from '@/stores';

export function useAdministrator(): boolean {
  return useAuthStore((state) => state.isSuperuser === true);
}

export interface Resource<T> {
  data: T | null;
  error: string;
  loading: boolean;
  refresh: () => Promise<void>;
}

/** Loads `load` on mount and whenever it changes, on the header refresh and,
 * with `pollMs`, on a timer while the tab is visible. A response that is not
 * the latest request is dropped; a failed poll keeps the data on screen. */
export function useResource<T>(load: () => Promise<T>, pollMs: number | null = null): Resource<T> {
  const [state, setState] = useState<{ data: T | null; error: string; source: unknown }>({
    data: null,
    error: '',
    source: null,
  });
  const latest = useRef(0);
  const refresh = useCallback(async () => {
    const version = ++latest.current;
    try {
      const data = await load();
      if (version === latest.current) setState({ data, error: '', source: load });
    } catch (error) {
      if (version !== latest.current) return;
      setState((prev) => ({
        data: prev.source === load ? prev.data : null,
        error: errorMessage(error),
        source: load,
      }));
    }
  }, [load]);
  useEffect(() => {
    void refresh();
    return () => {
      latest.current += 1;
    };
  }, [refresh]);
  useHeaderRefresh(refresh);
  useVisibleInterval(() => void refresh(), pollMs);
  const current = state.source === load;
  return {
    data: current ? state.data : null,
    error: current ? state.error : '',
    loading: !current,
    refresh,
  };
}
