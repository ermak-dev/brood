import { useTranslation } from 'react-i18next';
import { formatDateTimeValue } from '@/utils/format';
import { formatSince } from './executionAt';
import styles from '@/features/substrate/substrate.module.scss';

/** Сколько прошло с момента последнего исполнения (#268) -- словами локали,
 * рядом с исполнением; точный момент -- в подсказке и в `datetime`. Меньше
 * минуты -- «только что». now -- момент чтения строки (useReadMoment). */
export function ExecutionMoment({ at, now }: { at: number; now: number }) {
  const { t, i18n } = useTranslation();
  const exact = formatDateTimeValue(at, i18n.language);
  return (
    <time
      className={styles.muted}
      dateTime={new Date(at).toISOString()}
      title={t('access.execution_at', { time: exact })}
    >
      {formatSince(at, now, i18n.language) ?? t('access.execution_just_now')}
    </time>
  );
}
