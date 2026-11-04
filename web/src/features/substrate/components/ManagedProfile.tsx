import { useTranslation } from 'react-i18next';
import type { Actor, ActorTemplate } from '@/services/brood/substrate';
import { DASH } from '../format';
import styles from '../substrate.module.scss';

export function ManagedProfile({ entity }: { entity: Actor | ActorTemplate }) {
  const { t } = useTranslation();
  if (entity.provider !== 'proxmox') return null;
  const actor = 'generation' in entity ? (entity as Actor) : undefined;
  return (
    <div>
      <dl className={styles.details}>
        <dt>{t('actors.provider')}</dt>
        <dd>Proxmox</dd>
        <dt>{t('actors.profile')}</dt>
        <dd>{entity.profile || DASH}</dd>
        {actor && (
          <>
            <dt>CTID</dt>
            <dd>{actor.placement?.ctid ?? DASH}</dd>
            <dt>{t('actors.node')}</dt>
            <dd>{actor.placement?.node || DASH}</dd>
            <dt>{t('actors.pool')}</dt>
            <dd>{actor.placement?.pool || DASH}</dd>
          </>
        )}
      </dl>
      {actor?.status?.errorMessage && <p role="alert">{actor.status.errorMessage}</p>}
    </div>
  );
}
