import { useId, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Modal } from '@/components/ui/Modal';
import { useEscapeLayer } from './floating';
import styles from '../substrate.module.scss';

/** brood: подтверждение действия с последствиями. Для необратимого
 * (typedName) кнопка доступна только после ввода точного имени: случайный
 * Enter или клик не удалит данные. Escape закрывает только диалог, не
 * открытые под ним детали актора. Диалог остаётся смонтированным с
 * open=false, чтобы Modal вернул фокус туда, откуда его открыли. */
export function ConfirmDialog({
  open,
  title,
  children,
  confirmText,
  typedName,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  children: ReactNode;
  confirmText: string;
  typedName?: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const inputId = useId();
  const [typed, setTyped] = useState('');
  const ready = typedName === undefined || typed === typedName;
  useEscapeLayer(open, onCancel);

  return (
    <Modal open={open} title={title} onClose={onCancel}>
      <form
        className={styles.confirmBody}
        onSubmit={(event) => {
          event.preventDefault();
          if (ready) onConfirm();
        }}
      >
        {children}
        {typedName !== undefined && (
          <div className={styles.field}>
            <label className={styles.fieldLabel} htmlFor={inputId}>
              {t('actors.ops.type_name', { name: typedName })}
            </label>
            <input
              id={inputId}
              className={`input ${styles.mono}`}
              value={typed}
              autoComplete="off"
              autoCapitalize="off"
              spellCheck={false}
              onChange={(event) => setTyped(event.target.value)}
            />
          </div>
        )}
        <div className={styles.confirmActions}>
          <Button type="button" variant="ghost" onClick={onCancel}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" variant="danger" disabled={!ready}>
            {confirmText}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
