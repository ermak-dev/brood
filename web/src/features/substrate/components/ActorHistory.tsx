import { useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { sessionApi } from '@/services/brood/commands';
import { formatDateTimeValue } from '@/utils/format';
import { mergeHistory, type LoadedHistory } from '../historyView';
import { useSessionRead } from './useSessionRead';
import styles from '../substrate.module.scss';

const LIMIT = 50;

/** brood: история сессии от начала transcript, страницами по курсору. */
export function ActorHistory({ swarm, name }: { swarm: string; name: string }) {
  const { t, i18n } = useTranslation();
  const read = useSessionRead<LoadedHistory>();
  const data = read.data;
  const load = (first: boolean) =>
    read.run(
      () =>
        sessionApi.history(swarm, name, { cursor: first ? '' : (data?.cursor ?? ''), limit: LIMIT }),
      (previous, page) => mergeHistory(previous, page, first)
    );

  useEffect(() => {
    void load(true);
    // Первая страница — при открытии; дальше — по кнопке.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className={styles.section}>
      <div className={styles.consoleControls}>
        <Button
          type="button"
          variant="secondary"
          loading={read.loading && !data}
          onClick={() => void load(true)}
        >
          {t('actors.console.history.from_start')}
        </Button>
      </div>
      {read.error && (
        <p role="alert" className={`${styles.notice} ${styles.operationFailure}`}>
          {read.error}
        </p>
      )}
      {data?.restarted && <p className={styles.notice}>{t('actors.console.history.reset')}</p>}
      {data?.partial && <p className={styles.notice}>{t('actors.console.history.partial')}</p>}
      {data && data.entries.length === 0 && (
        <p className={styles.muted}>{t('actors.console.history.empty')}</p>
      )}
      {data && data.entries.length > 0 && (
        <ol className={styles.history} aria-label={t('actors.console.history.label', { name })}>
          {data.entries.map((entry, index) => (
            <li key={index} className={styles.historyEntry}>
              <div className={styles.historyMeta}>
                <span className={`${styles.badge} ${styles['tone-muted']}`}>
                  {t(`actors.console.history.kinds.${entry.kind}`)}
                </span>
                {entry.time && (
                  <time dateTime={entry.time}>{formatDateTimeValue(entry.time, i18n.language)}</time>
                )}
              </div>
              <div className={styles.historyText}>{entry.text}</div>
            </li>
          ))}
        </ol>
      )}
      {data?.hasMore && (
        <Button
          type="button"
          variant="secondary"
          loading={read.loading}
          className={styles.addButton}
          onClick={() => void load(false)}
        >
          {t('actors.console.history.more')}
        </Button>
      )}
    </div>
  );
}
