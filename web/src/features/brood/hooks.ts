import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useVisibleInterval } from '@/hooks/useInterval';
import { errorMessage } from '@/services/brood/http';
import { useAuthStore, useNotificationStore } from '@/stores';

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

export interface Confirmation {
  title: string;
  message: string;
  action: () => Promise<unknown>;
}

/** Runs mutations one at a time: success refreshes the page data, a refusal
 * shows the server's reason. `confirm` asks first, for destructive actions. */
export function useActions(refresh: () => Promise<unknown>) {
  const { t } = useTranslation();
  const notify = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const [busy, setBusy] = useState(false);
  const lock = useRef(false);
  const run = async (action: () => Promise<unknown>): Promise<boolean> => {
    if (lock.current) return false;
    lock.current = true;
    setBusy(true);
    try {
      await action();
      notify(t('brood.done'), 'success');
      return true;
    } catch (error) {
      notify(errorMessage(error), 'error');
      return false;
    } finally {
      lock.current = false;
      setBusy(false);
      void refresh();
    }
  };
  const confirm = ({ title, message, action }: Confirmation) =>
    showConfirmation({
      title,
      message,
      confirmText: title,
      cancelText: t('common.cancel'),
      variant: 'danger',
      onConfirm: async () => {
        await run(action);
      },
    });
  return { run, confirm, busy };
}
