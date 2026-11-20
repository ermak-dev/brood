import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Skeleton } from '@/components/ui/Skeleton';
import { useVisibleInterval } from '@/hooks/useInterval';
import styles from './table.module.scss';

export const DASH = '—';

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 31536000],
  ['month', 2592000],
  ['day', 86400],
  ['hour', 3600],
  ['minute', 60],
];

function relative(seconds: number, language?: string): string {
  let formatter: Intl.RelativeTimeFormat;
  try {
    formatter = new Intl.RelativeTimeFormat(language, { numeric: 'auto' });
  } catch {
    formatter = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
  }
  const unit = UNITS.find(([, size]) => Math.abs(seconds) >= size);
  return unit
    ? formatter.format(Math.trunc(seconds / unit[1]), unit[0])
    : formatter.format(0, 'second');
}

/** A moment as "3 hours ago"; the exact local time is in the title. */
export function RelativeTime({ value }: { value: string }) {
  const { i18n } = useTranslation();
  // The phrase ages with the page, not only with the next reload of the rows.
  const [now, setNow] = useState(Date.now);
  useVisibleInterval(() => setNow(Date.now()), 30000);
  const time = Date.parse(value);
  if (Number.isNaN(time)) return <>{DASH}</>;
  return (
    <time dateTime={value} title={new Date(time).toLocaleString(i18n.resolvedLanguage)}>
      {relative((time - now) / 1000, i18n.resolvedLanguage)}
    </time>
  );
}

/** One line cut with an ellipsis; the whole value is in the title. */
export function Truncate({
  text,
  mono = false,
  tone,
}: {
  text: string;
  mono?: boolean;
  tone?: 'problem';
}) {
  if (!text) return <>{DASH}</>;
  const classes = [styles.truncate, mono && styles.mono, tone && styles.problemText];
  return (
    <span className={classes.filter(Boolean).join(' ')} title={text}>
      {text}
    </span>
  );
}

/** A cell whose value is still being fetched after the row itself. */
export function Pending<T>({
  value,
  children,
}: {
  value: T | undefined;
  children: (value: T) => ReactNode;
}) {
  return value === undefined ? <Skeleton width={56} height={12} /> : <>{children(value)}</>;
}
