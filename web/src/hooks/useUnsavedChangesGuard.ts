import { useCallback, useEffect, useLayoutEffect, useMemo, useRef } from 'react';
import type { BlockerFunction, Location } from 'react-router';
import { NavigationType, parsePath, useBlocker, useLocation } from 'react-router';
import {
  canCancelTraversal,
  historyStateWritesDenied,
  readHistoryIndex,
} from '@/router/initialHistoryEntry';
import { useNotificationStore } from '@/stores';

type ConfirmationVariant = 'danger' | 'primary' | 'secondary';

export type UnsavedChangesDialog = {
  title: string;
  message: string;
  confirmText: string;
  cancelText: string;
  variant?: ConfirmationVariant;
};

export type UseUnsavedChangesGuardOptions = {
  enabled?: boolean;
  shouldBlock: boolean | BlockerFunction;
  dialog: UnsavedChangesDialog;
  /** Пользователь подтвердил уход: черновик больше не нужен (useDraftRecovery). */
  onDiscard?: () => void;
};

function hashLocation(url: string): Location {
  const { pathname = '/', search = '', hash = '' } = parsePath(new URL(url).hash.slice(1));
  return { pathname, search, hash, state: null, key: '' };
}

// Защита работает тремя слоями:
//  - blocker роутера отменяет переходы роутера и Back/Forward между его записями;
//  - Navigation API отменяет Back/Forward браузера и набранный в адресной строке hash
//    до того, как они произошли, — там, где браузер разрешает отмену (event.cancelable);
//  - beforeunload предупреждает об уходе со страницы.
// Остальные переходы (например, history.back() с записи без idx) не отменить без
// угадывания положения в истории; их данные сохраняет useDraftRecovery.
export function useUnsavedChangesGuard(options: UseUnsavedChangesGuardOptions) {
  const { enabled = true, shouldBlock, dialog, onDiscard } = options;
  const { showConfirmation } = useNotificationStore();
  const lastBlockedRef = useRef<string>('');
  const allowNextNavigationUntilRef = useRef(0);
  const allowNextNavigationKeyRef = useRef('');
  // Индекс роутера у записи этой страницы: с ним сравнивается цель Back/Forward.
  const currentIndexRef = useRef<number | undefined>(undefined);
  // Переход, подтверждённый через Navigation API, роутер не должен блокировать снова.
  const confirmedNavigationRef = useRef(false);
  const onDiscardRef = useRef(onDiscard);
  const location = useLocation();

  useLayoutEffect(() => {
    onDiscardRef.current = onDiscard;
  }, [onDiscard]);

  useLayoutEffect(() => {
    currentIndexRef.current = readHistoryIndex(window.history.state);
    confirmedNavigationRef.current = false;
  }, [location.key]);

  const allowNextNavigation = useCallback(() => {
    // Allow one programmatic navigation after successful save.
    // A short window is used to avoid stale flags lingering when no navigation happens.
    allowNextNavigationUntilRef.current = Date.now() + 2_000;
    allowNextNavigationKeyRef.current = '';
  }, []);

  const allowNavigationTo = useCallback((nextLocationKey: string) => {
    allowNextNavigationUntilRef.current = Date.now() + 2_000;
    allowNextNavigationKeyRef.current = nextLocationKey;
  }, []);

  const shouldBlockFunction = useCallback<BlockerFunction>(
    (args) => {
      if (!enabled) return false;
      const now = Date.now();

      if (allowNextNavigationUntilRef.current > now) {
        const nextKey = `${args.nextLocation.pathname}${args.nextLocation.search}${args.nextLocation.hash}`;
        if (!allowNextNavigationKeyRef.current) {
          allowNextNavigationKeyRef.current = nextKey;
        }
        if (allowNextNavigationKeyRef.current === nextKey) {
          return false;
        }
      } else if (allowNextNavigationUntilRef.current !== 0) {
        allowNextNavigationUntilRef.current = 0;
        allowNextNavigationKeyRef.current = '';
      }

      return typeof shouldBlock === 'function' ? shouldBlock(args) : shouldBlock;
    },
    [enabled, shouldBlock]
  );

  const routerBlocker = useCallback<BlockerFunction>(
    (args) => {
      if (args.historyAction === NavigationType.Pop) {
        if (confirmedNavigationRef.current) {
          confirmedNavigationRef.current = false;
          return false;
        }
        // Во время popstate history.state уже принадлежит цели перехода.
        const reliable = canCancelTraversal(
          currentIndexRef.current,
          readHistoryIndex(window.history.state),
          historyStateWritesDenied()
        );
        if (!reliable) return false;
      }
      return shouldBlockFunction(args);
    },
    [shouldBlockFunction]
  );

  const blocker = useBlocker(routerBlocker);

  useEffect(() => {
    if (allowNextNavigationUntilRef.current === 0) return;
    allowNextNavigationUntilRef.current = 0;
    allowNextNavigationKeyRef.current = '';
  }, [location.key]);

  const blockedKey = useMemo(() => {
    if (blocker.state !== 'blocked' || !blocker.location) return '';
    return `${blocker.location.pathname}${blocker.location.search}${blocker.location.hash}`;
  }, [blocker.location, blocker.state]);

  useEffect(() => {
    if (blocker.state !== 'blocked') {
      lastBlockedRef.current = '';
      return;
    }

    if (!blockedKey || lastBlockedRef.current === blockedKey) {
      return;
    }
    lastBlockedRef.current = blockedKey;

    showConfirmation({
      title: dialog.title,
      message: dialog.message,
      confirmText: dialog.confirmText,
      cancelText: dialog.cancelText,
      variant: dialog.variant ?? 'danger',
      onConfirm: () => {
        onDiscardRef.current?.();
        blocker.proceed();
      },
      onCancel: () => blocker.reset(),
    });
  }, [blockedKey, blocker, dialog, showConfirmation]);

  // Navigation API: отмена до перехода не трогает ни адрес, ни индексы роутера.
  // Отменяется только то, что пользователь сделал в браузере. pushState роутера
  // отменять нельзя: роутер уже сменил своё состояние. Сценарный history.go()
  // (кнопка «Назад» панели) идёт через blocker: после отменённого сценарного
  // traverse Chrome молча игнорирует повторный history.go() к той же записи.
  useEffect(() => {
    const navigation = (window as { navigation?: Navigation }).navigation;
    if (!enabled || shouldBlock === false || !navigation) return;
    const handleNavigate = (event: NavigateEvent) => {
      const traverse = event.navigationType === 'traverse';
      const fragment =
        event.hashChange && (event.navigationType === 'push' || event.navigationType === 'replace');
      if (!event.userInitiated || !event.cancelable || !event.destination.sameDocument) return;
      if (!(traverse || fragment) || confirmedNavigationRef.current) return;
      const historyAction = traverse
        ? NavigationType.Pop
        : event.navigationType === 'replace'
          ? NavigationType.Replace
          : NavigationType.Push;
      const nextLocation = hashLocation(event.destination.url);
      if (!shouldBlockFunction({ currentLocation: location, nextLocation, historyAction })) return;
      event.preventDefault();
      const { key, url } = event.destination;
      const historyMode = event.navigationType === 'replace' ? 'replace' : 'push';
      showConfirmation({
        title: dialog.title,
        message: dialog.message,
        confirmText: dialog.confirmText,
        cancelText: dialog.cancelText,
        variant: dialog.variant ?? 'danger',
        onConfirm: () => {
          onDiscardRef.current?.();
          confirmedNavigationRef.current = true;
          const result = traverse
            ? navigation.traverseTo(key)
            : navigation.navigate(url, { history: historyMode });
          // Запись могла исчезнуть; без перехода флаг не должен пропустить следующий.
          result.committed?.catch(() => {
            confirmedNavigationRef.current = false;
          });
          result.finished?.catch(() => undefined);
        },
      });
    };
    navigation.addEventListener('navigate', handleNavigate);
    return () => navigation.removeEventListener('navigate', handleNavigate);
  }, [dialog, enabled, location, shouldBlock, shouldBlockFunction, showConfirmation]);

  useEffect(() => {
    if (!enabled || shouldBlock !== true) return;
    const handleBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', handleBeforeUnload);
    return () => window.removeEventListener('beforeunload', handleBeforeUnload);
  }, [enabled, shouldBlock]);

  return { allowNextNavigation, allowNavigationTo };
}
