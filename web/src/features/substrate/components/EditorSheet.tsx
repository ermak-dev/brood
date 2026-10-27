import { useCallback, useLayoutEffect, useRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Sheet, type SheetSize } from '@/components/ui/Sheet';
import { useNotificationStore } from '@/stores';

interface EditorSheetProps {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  eyebrow?: ReactNode;
  description?: ReactNode;
  size?: SheetSize;
  /** Without onSave the sheet is read-only: just Close. */
  onSave?: () => void;
  saveLabel?: string;
  canSave?: boolean;
  saving?: boolean;
  /** Unsaved edits: closing asks first, as the upstream sheets do. */
  dirty?: boolean;
  /** Extra footer buttons on the left (e.g. "Duplicate" on a read-only view). */
  extra?: ReactNode;
  children: ReactNode;
}

/** brood: the side sheet every Substrate editor and details view opens in --
 * full width on a phone, Cancel/Save in the footer. */
export function EditorSheet({
  open,
  onClose,
  title,
  eyebrow,
  description,
  size = 'md',
  onSave,
  saveLabel,
  canSave = true,
  saving = false,
  dirty = false,
  extra,
  children,
}: EditorSheetProps) {
  const { t } = useTranslation();
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);

  // Sheet перезапускает показ, если onClose сменился посреди анимации закрытия,
  // а родитель отдаёт новую функцию на каждом рендере (подтверждение закрытия
  // как раз перерисовывает страницу) -- и панель после «Не сохранять» не
  // закрывалась. Стабильная обёртка снимает гонку у всех редакторов разом.
  const onCloseRef = useRef(onClose);
  useLayoutEffect(() => {
    onCloseRef.current = onClose;
  });
  const stableClose = useCallback(() => onCloseRef.current(), []);

  const confirmClose = useCallback((): Promise<boolean> => {
    if (!dirty || saving) return Promise.resolve(true);
    return new Promise<boolean>((resolve) => {
      showConfirmation({
        title: t('substrate.unsaved_title'),
        message: t('substrate.unsaved_message'),
        confirmText: t('substrate.unsaved_discard'),
        cancelText: t('substrate.unsaved_keep'),
        variant: 'danger',
        onConfirm: () => resolve(true),
        onCancel: () => resolve(false),
      });
    });
  }, [dirty, saving, showConfirmation, t]);

  const close = useCallback(async () => {
    if (await confirmClose()) stableClose();
  }, [confirmClose, stableClose]);

  return (
    <Sheet
      open={open}
      onClose={stableClose}
      confirmClose={confirmClose}
      closeDisabled={saving}
      size={size}
      eyebrow={eyebrow}
      title={title}
      description={description}
      footer={
        <>
          {extra && <div style={{ marginRight: 'auto', display: 'flex', gap: 8 }}>{extra}</div>}
          <Button variant="secondary" onClick={() => void close()} disabled={saving}>
            {onSave ? t('common.cancel') : t('common.close')}
          </Button>
          {onSave && (
            <Button onClick={onSave} disabled={!canSave} loading={saving}>
              {saveLabel ?? t('substrate.save')}
            </Button>
          )}
        </>
      }
    >
      {children}
    </Sheet>
  );
}
