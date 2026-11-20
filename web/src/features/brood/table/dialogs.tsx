import {
  useEffect,
  useId,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
  type RefObject,
} from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Modal } from '@/components/ui/Modal';
import { errorMessage } from '@/services/brood/http';
import { useNotificationStore } from '@/stores';
import styles from './table.module.scss';

/** The shared Modal focuses its close button; a form starts in its first
 * field and a confirmation on Cancel, so this runs right after it. */
function useInitialFocus(open: boolean, root: RefObject<HTMLElement | null>, selector: string) {
  useEffect(() => {
    if (!open) return;
    const timer = window.setTimeout(
      () => root.current?.querySelector<HTMLElement>(selector)?.focus(),
      30
    );
    return () => window.clearTimeout(timer);
  }, [open, root, selector]);
}

/** A labelled control that is not an Input: the label points at its id. */
export function Field({
  label,
  required = false,
  hint,
  error,
  children,
}: {
  label: string;
  required?: boolean;
  hint?: ReactNode;
  error?: string;
  children: (id: string) => ReactNode;
}) {
  const id = useId();
  return (
    <div className={styles.field}>
      <label className={`${styles.fieldLabel} ${required ? styles.required : ''}`} htmlFor={id}>
        {label}
      </label>
      {children(id)}
      {hint && <p className={styles.fieldHint}>{hint}</p>}
      {error && (
        <p className={styles.fieldError} role="alert">
          {error}
        </p>
      )}
    </div>
  );
}

/** Create or edit in a dialog on the page of the table. Enter submits,
 * Escape cancels. A refusal is shown inside with everything typed kept;
 * success closes the dialog, confirms with a notification and runs `after`
 * (the table refresh). */
export function FormDialog({
  open,
  close,
  title,
  submitLabel,
  valid,
  submit,
  success,
  after,
  width,
  children,
}: {
  open: boolean;
  close: () => void;
  title: string;
  submitLabel: string;
  valid: boolean;
  /** Rejects with the server's reason. */
  submit: () => Promise<unknown>;
  success: string;
  after: () => unknown;
  width?: number;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  const notify = useNotificationStore((state) => state.showNotification);
  const form = useRef<HTMLFormElement | null>(null);
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState('');
  useInitialFocus(open, form, 'input:not([disabled]), textarea, button[aria-haspopup]');
  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!valid || saving) return;
    setSaving(true);
    setFailure('');
    try {
      await submit();
    } catch (error) {
      setFailure(errorMessage(error));
      void after();
      return;
    } finally {
      setSaving(false);
    }
    notify(success, 'success');
    close();
    void after();
  };
  return (
    <Modal open={open} title={title} onClose={close} closeDisabled={saving} width={width}>
      <form ref={form} className={styles.form} noValidate onSubmit={(event) => void save(event)}>
        {children}
        {failure && (
          <p className={styles.failure} role="alert">
            {failure}
          </p>
        )}
        <div className={styles.formActions}>
          <Button type="button" variant="ghost" onClick={close} disabled={saving}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" disabled={!valid} loading={saving}>
            {submitLabel}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

export interface Confirmation {
  title: string;
  /** Names the object and states the consequence. */
  message: string;
  confirmLabel: string;
  /** Rejects with the server's reason, which the dialog then shows. */
  action: () => Promise<unknown>;
}

/** Asks before a destructive action. Focus starts on Cancel; a refusal of
 * the server stays in the dialog. */
export function ConfirmDialog({
  request,
  close,
}: {
  request: Confirmation | null;
  close: () => void;
}) {
  const { t } = useTranslation();
  const body = useRef<HTMLDivElement | null>(null);
  // The last request stays on screen while the dialog plays its closing.
  const [shown, setShown] = useState(request);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState('');
  if (request && request !== shown) {
    setShown(request);
    setFailure('');
  }
  useInitialFocus(request !== null, body, 'button');
  const confirm = async () => {
    if (!shown || busy) return;
    setBusy(true);
    setFailure('');
    try {
      await shown.action();
      close();
    } catch (error) {
      setFailure(errorMessage(error));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal open={request !== null} title={shown?.title} onClose={close} closeDisabled={busy}>
      <div ref={body} className={styles.form}>
        <p className={styles.confirmText}>{shown?.message}</p>
        {failure && (
          <p className={styles.failure} role="alert">
            {failure}
          </p>
        )}
        <div className={styles.formActions}>
          <Button variant="ghost" onClick={close} disabled={busy}>
            {t('common.cancel')}
          </Button>
          <Button variant="danger" onClick={() => void confirm()} loading={busy}>
            {shown?.confirmLabel}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
