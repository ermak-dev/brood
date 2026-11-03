import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import type { UnknownOutcome } from '../actorOperations';
import styles from '../substrate.module.scss';

/** brood: неизвестные исходы действий. Остаются до явного закрытия: эффект
 * мог произойти, и повтор без проверки может повторить его. Панель сама
 * ничего не повторяет. */
export function ActorOutcomeNotices({
  items,
  onDismiss,
}: {
  items: UnknownOutcome[];
  onDismiss: (item: UnknownOutcome) => void;
}) {
  const { t } = useTranslation();
  if (!items.length) return null;
  return (
    <>
      {items.map((item) => (
        <section
          key={`${item.name}:${item.action}`}
          role="alert"
          className={`${styles.notice} ${styles.unknownNotice}`}
        >
          <strong>
            {t('actors.ops.unconfirmed_title', {
              action: t(`actors.ops.${item.action}.label`),
              name: item.name,
            })}
          </strong>
          <p>{t('actors.ops.unconfirmed_body')}</p>
          {item.detail && <p className={styles.mono}>{item.detail}</p>}
          <Button type="button" variant="ghost" size="sm" onClick={() => onDismiss(item)}>
            {t('actors.ops.dismiss')}
          </Button>
        </section>
      ))}
    </>
  );
}
