import { useTranslation } from 'react-i18next';
import { toneOf } from '../format';
import styles from '../substrate.module.scss';

interface StateBadgeProps {
  /** Short state ('running'), translated under `namespace`. */
  state: string;
  namespace: 'actors.state' | 'workers.state';
}

export function StateBadge({ state, namespace }: StateBadgeProps) {
  const { t } = useTranslation();
  if (!state) return <span className={styles.muted}>—</span>;
  return (
    <span className={`${styles.badge} ${styles[`tone-${toneOf(state)}`]}`}>
      {t(`${namespace}.${state}`, state)}
    </span>
  );
}
