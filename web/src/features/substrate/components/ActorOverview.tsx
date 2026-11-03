import { useTranslation } from 'react-i18next';
import type { Actor } from '@/services/brood/substrate';
import { DASH, formatCpu, formatMemory } from '../format';
import { ManagedProfile } from './ManagedProfile';
import { StateBadge } from './StateBadge';
import styles from '../substrate.module.scss';

/** brood: что известно о managed-акторе: состояние и намерение, origin и
 * путь клона, поколение, роль, ресурсы; размещение и ограничения профиля —
 * общий ManagedProfile. */
export function ActorOverview({ actor }: { actor: Actor }) {
  const { t } = useTranslation();
  const resources = actor.templateDefinition?.resources;
  const mode = actor.originMode || 'none';
  return (
    <div className={styles.section}>
      <dl className={styles.details}>
        <dt>{t('actors.console.overview.phase')}</dt>
        <dd>
          <StateBadge state={actor.phase ?? ''} namespace="actors.managed_state" />
        </dd>
        <dt>{t('actors.console.overview.desired')}</dt>
        <dd>{actor.desired ? t(`actors.console.overview.desired_values.${actor.desired}`) : DASH}</dd>
        <dt>{t('actors.console.overview.role')}</dt>
        <dd>{actor.role ? t(`actors.console.overview.roles.${actor.role}`, actor.role) : DASH}</dd>
        <dt>{t('actors.console.overview.origin')}</dt>
        <dd className={actor.origin ? styles.mono : undefined}>
          {actor.origin || t('actors.console.overview.no_origin')}
        </dd>
        <dt>{t('actors.console.overview.origin_mode')}</dt>
        <dd>{t(`actors.console.overview.modes.${mode}`)}</dd>
        <dt>{t('actors.console.overview.generation')}</dt>
        <dd>
          <span className={styles.mono}>{actor.generation ?? DASH}</span>
          <span className={styles.fieldHint}> {t('actors.console.overview.generation_hint')}</span>
        </dd>
        <dt>{t('actors.console.overview.resources')}</dt>
        <dd>
          {resources
            ? `${formatCpu(resources.cpuMillicores)} · ${formatMemory(resources.memoryBytes)} · ${t(
                'actors.console.overview.disk',
                { size: formatMemory(resources.diskBytes) }
              )}`
            : DASH}
        </dd>
      </dl>
      <ManagedProfile entity={actor} />
    </div>
  );
}
