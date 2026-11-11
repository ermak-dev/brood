import { useTranslation } from 'react-i18next';
import type { Actor, ActorTemplate } from '@/services/brood/substrate';
import { ObjectLink, RelatedLink } from '@/features/objects/ObjectLink';
import { actorTemplate, type SwarmScope } from '@/features/objects/relations';
import { Section } from './form';
import styles from '../substrate.module.scss';

/** Связи актора (#195): его рой и шаблон -- ссылками. Воркер и пул стоят в
 * сведениях о размещении рядом с узлом, там они и ссылки. Шаблона нет в
 * списке роя -- его имя текстом. */
export function ActorRelations({
  actor,
  scope,
  swarmName,
  templates,
}: {
  actor: Actor;
  scope: SwarmScope;
  swarmName: string;
  /** Шаблоны роя; null -- ещё не прочитаны. */
  templates: ActorTemplate[] | null;
}) {
  const { t } = useTranslation();
  return (
    <Section title={t('relations.title')}>
      <dl className={styles.details} data-relations="">
        <dt>{t('relations.swarm')}</dt>
        <dd>
          <ObjectLink object={{ kind: 'swarm', id: scope.swarm }}>{swarmName}</ObjectLink>
        </dd>
        <dt>{t('actors.template')}</dt>
        <dd>
          <RelatedLink related={actorTemplate(actor, scope, templates)} />
        </dd>
      </dl>
    </Section>
  );
}
