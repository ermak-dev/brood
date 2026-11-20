import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
  type RefObject,
} from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Modal } from '@/components/ui/Modal';
import { Sheet, type SheetSize } from '@/components/ui/Sheet';
import { errorMessage } from '@/services/brood/http';
import { useNotificationStore } from '@/stores';
import type { Icon } from '../ui';
import styles from './table.module.scss';

const FIRST_FIELD =
  'input:not([disabled]):not([type="checkbox"]), textarea, button[aria-haspopup]:not([disabled])';

/** The shared Sheet and Modal focus their close button; a form starts in
 * its first field and a confirmation on Cancel, so this runs right after. */
function useInitialFocus(open: boolean, root: RefObject<HTMLElement | null>, selector: string) {
  useEffect(() => {
    if (!open) return;
    const timer = window.setTimeout(() => {
      const first = root.current?.querySelector<HTMLElement>(selector);
      first?.focus();
      // A prefilled value is selected: typing replaces it, Enter keeps it.
      if (first instanceof HTMLInputElement && first.value) first.select();
    }, 40);
    return () => window.clearTimeout(timer);
  }, [open, root, selector]);
}

/** A titled group of fields inside a long form. */
export function FormSection({
  title,
  hint,
  children,
}: {
  title: string;
  hint?: ReactNode;
  children: ReactNode;
}) {
  return (
    <fieldset className={styles.formSection}>
      <legend>{title}</legend>
      {hint && <p className={styles.fieldHint}>{hint}</p>}
      {children}
    </fieldset>
  );
}

/** A labelled control that is not an Input: the label points at its id,
 * the help line stays under it and the message of the check comes last. */
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

/** Create or edit in the side sheet of the page: Cancel and Save in the
 * footer, focus in the first field, Enter submits. Save is never just
 * disabled: `check` shows what is wrong next to each field. A refusal of
 * the server stays inside with everything typed kept; closing with unsaved
 * input asks first. Success closes the sheet, confirms with a notification
 * and runs `after` (the table refresh). */
export function FormSheet({
  open,
  close,
  title,
  icon: Mark,
  description,
  submitLabel,
  check,
  dirty,
  submit,
  success,
  after,
  size = 'md',
  children,
}: {
  open: boolean;
  close: () => void;
  title: string;
  icon?: Icon;
  description?: ReactNode;
  submitLabel: string;
  /** Marks every field as checked; false keeps the sheet open. */
  check: () => boolean;
  dirty: boolean;
  /** Rejects with the server's reason. */
  submit: () => Promise<unknown>;
  success: string;
  after: () => unknown;
  size?: SheetSize;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  const notify = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const formId = useId();
  const form = useRef<HTMLFormElement | null>(null);
  const failureNote = useRef<HTMLParagraphElement | null>(null);
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState('');
  useInitialFocus(open, form, FIRST_FIELD);

  // The Sheet restarts its closing when onClose changes mid-animation, and
  // the parent hands a new function on every render: one stable wrapper.
  const closeRef = useRef(close);
  useLayoutEffect(() => {
    closeRef.current = close;
  });
  const stableClose = useCallback(() => closeRef.current(), []);

  const confirmClose = useCallback((): Promise<boolean> => {
    if (!dirty || saving) return Promise.resolve(true);
    return new Promise<boolean>((resolve) => {
      showConfirmation({
        title: t('brood.form.unsaved_title'),
        message: t('brood.form.unsaved_message'),
        confirmText: t('brood.form.unsaved_discard'),
        cancelText: t('brood.form.unsaved_keep'),
        variant: 'danger',
        onConfirm: () => resolve(true),
        onCancel: () => resolve(false),
      });
    });
  }, [dirty, saving, showConfirmation, t]);

  useEffect(() => {
    if (failure) failureNote.current?.scrollIntoView({ block: 'nearest' });
  }, [failure]);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (saving) return;
    if (!check()) {
      // The messages render first; then the first wrong field takes focus.
      window.setTimeout(
        () => form.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus(),
        0
      );
      return;
    }
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
    <Sheet
      open={open}
      onClose={stableClose}
      confirmClose={confirmClose}
      closeDisabled={saving}
      size={size}
      title={
        <span className={styles.sheetTitle}>
          {Mark && <Mark size={18} />}
          {title}
        </span>
      }
      description={description}
      footer={
        <>
          <Button
            variant="secondary"
            disabled={saving}
            onClick={() => void confirmClose().then((leave) => leave && stableClose())}
          >
            {t('common.cancel')}
          </Button>
          <Button type="submit" form={formId} loading={saving}>
            {submitLabel}
          </Button>
        </>
      }
    >
      <form
        id={formId}
        ref={form}
        className={styles.form}
        noValidate
        onSubmit={(event) => void save(event)}
      >
        {children}
        {failure && (
          <p ref={failureNote} className={styles.failure} role="alert">
            {failure}
          </p>
        )}
      </form>
    </Sheet>
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

/** Asks before an action with consequences. Focus starts on Cancel; a
 * refusal of the server stays in the dialog. */
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
