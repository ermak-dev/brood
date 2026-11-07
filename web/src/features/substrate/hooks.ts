import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useNotificationStore } from '@/stores';
import { errorMessage } from '@/services/brood/http';

/** brood: load once and on demand (the header's refresh button too). Pass a
 * memoized loader; null waits -- e.g. until a project is chosen. */
export function useResource<T>(
  load: (() => Promise<T>) | null,
  {
    headerRefresh = true,
    trackRefresh = false,
  }: { headerRefresh?: boolean; trackRefresh?: boolean } = {}
) {
  const [refreshing, setRefreshing] = useState(false);
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Номер последнего запроса: ответ на прежний (другой проект, другой скоуп),
  // пришедший позже, не ложится поверх текущего.
  const latest = useRef(0);

  const refresh = useCallback(async () => {
    if (!load) return;
    const request = ++latest.current;
    if (trackRefresh) setRefreshing(true);
    try {
      const loaded = await load();
      if (request !== latest.current) return;
      setData(loaded);
      setError(null);
    } catch (e) {
      if (request !== latest.current) return;
      setError(errorMessage(e));
    } finally {
      if (trackRefresh && request === latest.current) setRefreshing(false);
    }
  }, [load, trackRefresh]);

  useEffect(() => {
    setData(null);
    setError(null);
    void refresh();
  }, [refresh]);

  useHeaderRefresh(refresh, Boolean(load) && headerRefresh);

  return { data, error, loading: refreshing || (data === null && error === null), refresh };
}

interface ConfirmedAction {
  title: string;
  message: string;
  confirmText: string;
  run: () => Promise<unknown>;
  success: string;
}

/** brood: a mutation with the one outcome every page shows -- a success or the
 * server's reason as a notification, then a reload. `run` reports whether the
 * mutation went through, so an editor knows when to close; `confirm` asks
 * first -- every confirmed action here destroys something. */
export function useActions(refresh: () => Promise<void>) {
  const { t } = useTranslation();
  const notify = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);

  const run = useCallback(
    async (mutation: () => Promise<unknown>, success: string): Promise<boolean> => {
      try {
        await mutation();
      } catch (e) {
        notify(errorMessage(e), 'error');
        return false;
      }
      notify(success, 'success');
      await refresh();
      return true;
    },
    [notify, refresh]
  );

  const confirm = useCallback(
    ({ title, message, confirmText, run: mutation, success }: ConfirmedAction) =>
      showConfirmation({
        title,
        message,
        confirmText,
        cancelText: t('common.cancel'),
        variant: 'danger',
        onConfirm: async () => {
          await run(mutation, success);
        },
      }),
    [run, showConfirmation, t]
  );

  return { run, confirm };
}
