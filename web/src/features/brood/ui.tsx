import { Fragment, useEffect, useRef, useState, type ComponentType, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { IconCheck, IconCopy, type IconProps } from '@/components/ui/icons';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { copyToClipboard } from '@/utils/clipboard';
import styles from './brood.module.scss';

export type BadgeTone = 'attention' | 'progress' | 'problem' | 'neutral';
export type Icon = ComponentType<IconProps>;

/** A state in words, with its colour and, when given, its own icon; a
 * state that is still changing spins instead. `title` carries what does not
 * fit (the error of an agent). */
export function StatusBadge({
  tone,
  icon: Mark,
  busy = false,
  title,
  children,
}: {
  tone: BadgeTone;
  icon?: Icon;
  busy?: boolean;
  title?: string;
  children: ReactNode;
}) {
  const marked = busy || Mark !== undefined;
  return (
    <span
      className={`${styles.badge} ${styles[`tone-${tone}`]} ${marked ? styles.badgeMarked : ''}`}
      data-tone={tone}
      title={title}
    >
      {busy ? (
        <span className={`loading-spinner ${styles.badgeSpinner}`} aria-hidden="true" />
      ) : (
        Mark && <Mark size={12} />
      )}
      {children}
    </span>
  );
}

/** Copies `value` and confirms it for a moment in place of the icon. With
 * `label` the button carries the text too (a command line), otherwise it is
 * the small icon next to a value. */
export function CopyButton({ value, label }: { value: string; label?: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | null>(null);
  useEffect(() => () => window.clearTimeout(timer.current ?? undefined), []);
  const copy = async () => {
    if (!(await copyToClipboard(value))) return;
    setCopied(true);
    window.clearTimeout(timer.current ?? undefined);
    timer.current = window.setTimeout(() => setCopied(false), 1600);
  };
  const name = t('brood.copy_value', { value: label ?? value });
  return (
    <button
      type="button"
      className={`${styles.copyButton} ${copied ? styles.copied : ''}`}
      aria-label={name}
      title={copied ? t('brood.copied') : name}
      onClick={(event) => {
        event.stopPropagation();
        void copy();
      }}
    >
      {copied ? <IconCheck size={13} /> : <IconCopy size={13} />}
      <span className={styles.copyNote} aria-live="polite">
        {copied ? t('brood.copied') : ''}
      </span>
    </button>
  );
}

/** A value people copy: the text, mono, with its copy button. `shown` is
 * a shorter form on screen (a commit); the whole value is what is copied. */
export function Copyable({ value, shown }: { value: string; shown?: string }) {
  if (!value) return <>—</>;
  return (
    <span className={styles.copyable}>
      <span className={styles.mono}>{shown ?? value}</span>
      <CopyButton value={value} />
    </span>
  );
}

/** Used of total as a small bar with the numbers next to it. */
export function UsageBar({
  label,
  used,
  total,
  unit = '',
}: {
  label: string;
  used: number;
  total: number;
  unit?: string;
}) {
  const share = total > 0 ? Math.min(1, used / total) : 0;
  const round = (value: number) => Math.round(value * 10) / 10;
  return (
    <div className={styles.usage}>
      <span className={styles.usageLabel}>{label}</span>
      <span
        className={`${styles.usageTrack} ${share >= 0.9 ? styles.usageFull : ''}`}
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={round(total)}
        aria-valuenow={round(used)}
      >
        <span style={{ width: `${share * 100}%` }} />
      </span>
      <span className={styles.usageNumbers}>
        {round(used)} / {round(total)}
        {unit && ` ${unit}`}
      </span>
    </div>
  );
}

/** The `bro` commands that do the same from a terminal, each with a copy
 * button. Only commands that exist in go/cmd/bro and go/brood/command. */
export function Commands({ items }: { items: { label: string; command: string }[] }) {
  return (
    <ul className={styles.commands}>
      {items.map((item) => (
        <li key={item.command}>
          <span className={styles.commandLabel}>{item.label}</span>
          <span className={styles.commandLine}>
            <code>{item.command}</code>
            <CopyButton value={item.command} />
          </span>
        </li>
      ))}
    </ul>
  );
}

/** Page title with its icon, a one-line description or a mono meta line,
 * and the page actions on the right. */
export function PageHeader({
  title,
  icon: Mark,
  description,
  meta = [],
  actions,
}: {
  title: ReactNode;
  icon?: Icon;
  description?: string;
  meta?: ReactNode[];
  actions?: ReactNode;
}) {
  const items = meta.filter(Boolean);
  return (
    <header className={styles.header}>
      <div className={styles.copy}>
        <h1 className={styles.title}>
          {Mark && (
            <span className={styles.titleIcon}>
              <Mark size={22} />
            </span>
          )}
          {title}
        </h1>
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
