import { Fragment, useId, useState, type FormEvent, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
import styles from './brood.module.scss';

export const DASH = '—';

export type BadgeTone = 'attention' | 'progress' | 'problem' | 'neutral';

export function StatusBadge({ tone, children }: { tone: BadgeTone; children: ReactNode }) {
  return (
    <span className={`${styles.badge} ${styles[`tone-${tone}`]}`} data-tone={tone}>
      {children}
    </span>
  );
}

export interface Crumb {
  to: string;
  label: string;
}

/** Page title with the path that leads to it (Projects / project / swarm),
 * a mono meta line and the page actions. */
export function PageHeader({
  title,
  crumbs = [],
  meta = [],
  actions,
}: {
  title: ReactNode;
  crumbs?: Crumb[];
  meta?: ReactNode[];
  actions?: ReactNode;
}) {
  const { t } = useTranslation();
  const items = meta.filter(Boolean);
  return (
    <header className={styles.header}>
      <div className={styles.copy}>
        {crumbs.length > 0 && (
          <nav className={styles.crumbs} aria-label={t('brood.breadcrumb')}>
            {crumbs.map((crumb) => (
              <Fragment key={crumb.to}>
                <Link to={crumb.to}>{crumb.label}</Link>
                <span aria-hidden="true">/</span>
              </Fragment>
            ))}
          </nav>
        )}
        <h1 className={styles.title}>{title}</h1>
        {items.length > 0 && (
          <p className={styles.meta}>
            {items.map((item, index) => (
              <Fragment key={index}>
                {index > 0 && (
                  <span className={styles.metaDot} aria-hidden="true">
                    ·
                  </span>
                )}
                <span>{item}</span>
              </Fragment>
            ))}
          </p>
        )}
      </div>
      {actions && <div className={styles.actions}>{actions}</div>}
    </header>
  );
}

export function Section({
  title,
  hint,
  action,
  nested = false,
  children,
}: {
  title: string;
  hint?: ReactNode;
  action?: ReactNode;
  /** A section inside a section: its heading is one level lower. */
  nested?: boolean;
  children: ReactNode;
}) {
  const Heading = nested ? 'h3' : 'h2';
  return (
    <section className={styles.section}>
      <div className={styles.sectionHead}>
        <div>
          <Heading className={styles.sectionTitle}>{title}</Heading>
          {hint && <p className={styles.sectionHint}>{hint}</p>}
        </div>
        {action}
      </div>
      {children}
    </section>
  );
}

/** A labelled control that is not an Input: the label points at its id. */
export function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: ReactNode;
  children: (id: string) => ReactNode;
}) {
  const id = useId();
  return (
    <div className={styles.field}>
      <label className={styles.fieldLabel} htmlFor={id}>
        {label}
      </label>
      {children(id)}
      {hint && <p className={styles.fieldHint}>{hint}</p>}
    </div>
  );
}

/** Spinner until the first answer, the refusal with a retry after a failed
 * one; once data is on screen a failed refresh only adds the reason. */
export function LoadState({
  loading,
  error,
  ready,
  retry,
  children,
}: {
  loading: boolean;
  error: string;
  ready: boolean;
  retry: () => Promise<void>;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  if (loading) return <LoadingSpinner />;
  if (!ready)
    return (
      <div className={styles.failure} role="alert">
        <p>{error}</p>
        <Button variant="secondary" size="sm" onClick={() => void retry()}>
          {t('common.refresh')}
        </Button>
      </div>
    );
  return (
    <>
      {error && (
        <p className={styles.failure} role="alert">
          {error}
        </p>
      )}
      {children}
    </>
  );
}

/** A dialog with a form: Enter submits, the dialog closes when `submit`
 * resolves to true and stays open with the fields intact otherwise. It stays
 * mounted while closed, so the Modal can return focus to where it came from. */
export function FormModal({
  open,
  title,
  submitLabel,
  canSubmit,
  submit,
  close,
  width,
  children,
}: {
  open: boolean;
  title: string;
  submitLabel: string;
  canSubmit: boolean;
  submit: () => Promise<boolean>;
  close: () => void;
  width?: number;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  const [saving, setSaving] = useState(false);
  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!canSubmit || saving) return;
    setSaving(true);
    try {
      if (await submit()) close();
    } finally {
      setSaving(false);
    }
  };
  return (
    <Modal open={open} title={title} onClose={close} closeDisabled={saving} width={width}>
      <form className={styles.form} onSubmit={(event) => void save(event)}>
        {children}
        <div className={styles.formActions}>
          <Button type="button" variant="ghost" onClick={close} disabled={saving}>
            {t('common.cancel')}
          </Button>
          <Button type="submit" disabled={!canSubmit} loading={saving}>
            {submitLabel}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
