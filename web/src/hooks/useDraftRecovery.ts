import { useCallback, useEffect, useLayoutEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { unsavedDrafts, type DraftPersistence } from '@/services/storage/unsavedDrafts';
import { useNotificationStore } from '@/stores';

export type UseDraftRecoveryOptions<T> = {
  /** Идентичность редактора (страница и объект); пустая строка отключает. */
  key: string;
  /** Исходные данные загружены: предложение восстановить сравнивает с ними. */
  ready: boolean;
  /** 'memory' для данных с секретами: sessionStorage их не получает. */
  persistence: DraftPersistence;
  /** Текущие несохранённые правки или null, если хранить нечего. */
  read: () => T | null;
  /** Проверка сохранённого значения; null — восстанавливать нечего. */
  parse: (value: unknown) => T | null;
  restore: (draft: T) => void;
};

const signature = (value: unknown) => (value === null ? null : JSON.stringify(value));

/**
 * Черновик редактора вне жизненного цикла страницы. Переход, который нельзя
 * отменить (Back с записи без индекса роутера, отказ браузера отменить
 * traverse), размонтирует редактор; его правки остаются в unsavedDrafts,
 * и при следующем открытии редактор предлагает их вернуть.
 * discard() — правки сохранены или пользователь отказался от них.
 */
export function useDraftRecovery<T>(options: UseDraftRecoveryOptions<T>) {
  const { key, ready } = options;
  const { t } = useTranslation();
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const latest = useRef(options);
  // Предложение ещё не принято и не отклонено: сохранённый черновик не трогаем.
  const resolvedRef = useRef(true);
  const offeredRef = useRef('');
  const discardedRef = useRef<string | null>(null);

  useLayoutEffect(() => {
    latest.current = options;
  });

  const keep = useCallback((draftKey: string, generation: number) => {
    if (!resolvedRef.current || generation !== unsavedDrafts.generation()) return;
    const { read, persistence } = latest.current;
    const value = read();
    const current = signature(value);
    if (current === null || current === discardedRef.current) unsavedDrafts.discard(draftKey);
    else unsavedDrafts.retain(draftKey, value, persistence);
  }, []);

  useEffect(() => {
    if (!key) return;
    const generation = unsavedDrafts.generation();
    resolvedRef.current = unsavedDrafts.read(key) === undefined;
    discardedRef.current = null;
    // Перезагрузку и закрытие вкладки переживает только черновик в sessionStorage.
    const handlePageHide = () => {
      if (latest.current.persistence === 'session') keep(key, generation);
    };
    window.addEventListener('pagehide', handlePageHide);
    return () => {
      window.removeEventListener('pagehide', handlePageHide);
      // latest уже несёт новый ключ, если редактор переключился на другой объект:
      // прежние правки он больше не держит, их судьбу решило подтверждение смены.
      if (latest.current.key === key) keep(key, generation);
      else if (resolvedRef.current) unsavedDrafts.discard(key);
    };
  }, [keep, key]);

  useEffect(() => {
    if (!key || !ready || resolvedRef.current || offeredRef.current === key) return;
    const draft = latest.current.parse(unsavedDrafts.read(key));
    if (draft === null) {
      unsavedDrafts.discard(key);
      resolvedRef.current = true;
      return;
    }
    offeredRef.current = key;
    const confirmation = {
      title: t('common.unsaved_draft_title'),
      message: t('common.unsaved_draft_message'),
      confirmText: t('common.unsaved_draft_restore'),
      cancelText: t('common.unsaved_draft_discard'),
      variant: 'primary' as const,
      onConfirm: () => {
        resolvedRef.current = true;
        latest.current.restore(draft);
      },
      onCancel: () => {
        resolvedRef.current = true;
        unsavedDrafts.discard(key);
      },
    };
    showConfirmation(confirmation);
    return () => {
      // Без ответа черновик остаётся сохранённым; диалог не переживает редактор.
      const store = useNotificationStore.getState();
      if (store.confirmation.options !== confirmation) return;
      store.hideConfirmation();
      offeredRef.current = '';
    };
  }, [key, ready, showConfirmation, t]);

  const discard = useCallback(() => {
    const { key: draftKey, read } = latest.current;
    discardedRef.current = signature(read());
    resolvedRef.current = true;
    if (draftKey) unsavedDrafts.discard(draftKey);
  }, []);

  return { discard };
}
