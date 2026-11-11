import { useTranslation } from 'react-i18next';
import type { Actor, ActorTemplate } from '@/services/brood/substrate';
import { useAdministrator } from '@/features/access/shared';
import { ObjectLink, RelatedList } from '@/features/objects/ObjectLink';
import {
  actorRelated,
  templateActors,
  templatePools,
  type SwarmScope,
} from '@/features/objects/relations';
import { canViewActorDetails } from '../actorLifecycle';
import { DASH } from '../format';
import { Section } from './form';
import styles from '../substrate.module.scss';

/** Акторы роя для связей шаблона: список или причина, почему его нет. */
export type SwarmActors = { list: Actor[] } | { error: string };

/** Связи шаблона (#195): рой, акторы, созданные из него, и пулы, где они
 * работают. Своего пула у шаблона в данных сервера нет, поэтому пулы -- его
 * акторов. Актор, чью панель пользователь не откроет (native у viewer), --
 * имя без ссылки; непрочитанные акторы -- причина, а не «ни одного». */
export function TemplateRelations({
  template,
  scope,
  swarmName,
  actors,
  canManage,
}: {
  template: ActorTemplate;
  scope: SwarmScope;
  swarmName: string;
  actors: SwarmActors;
  canManage: boolean;
}) {
  const { t } = useTranslation();
  const administrator = useAdministrator();
  const list = 'list' in actors ? actors.list : null;
  const used = list ? templateActors(template, list) : [];
  const pools = list ? templatePools(template, list, administrator) : [];
  return (
    <Section title={t('relations.title')}>
      <dl className={styles.details} data-relations="">
        <dt>{t('relations.swarm')}</dt>
        <dd>
          <ObjectLink object={{ kind: 'swarm', id: scope.swarm }}>{swarmName}</ObjectLink>
        </dd>
        <dt>{t('relations.actors')}</dt>
        <dd>
          {'error' in actors ? (
            t('relations.actors_failed', { reason: actors.error })
          ) : used.length ? (
            <RelatedList
              items={used.map((actor) =>
                actorRelated(actor, scope, canViewActorDetails(canManage, actor))
              )}
            />
          ) : (
            t('relations.no_actors')
          )}
        </dd>
        <dt>{t('relations.actor_pools')}</dt>
        <dd>{pools.length ? <RelatedList items={pools} /> : DASH}</dd>
      </dl>
    </Section>
  );
}
