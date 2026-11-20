import { Fragment, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import styles from './brood.module.scss';

export type BadgeTone = 'attention' | 'progress' | 'problem' | 'neutral';

export function StatusBadge({ tone, children }: { tone: BadgeTone; children: ReactNode }) {
  return (
    <span className={`${styles.badge} ${styles[`tone-${tone}`]}`} data-tone={tone}>
      {children}
    </span>
  );
}

/** Page title with a one-line description or a mono meta line, and the
 * page actions on the right. */
export function PageHeader({
  title,
  description,
  meta = [],
  actions,
}: {
  title: ReactNode;
  description?: string;
  meta?: ReactNode[];
  actions?: ReactNode;
}) {
  const items = meta.filter(Boolean);
  return (
    <header className={styles.header}>
      <div className={styles.copy}>
        <h1 className={styles.title}>{title}</h1>
        {description && <p className={styles.description}>{description}</p>}
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
