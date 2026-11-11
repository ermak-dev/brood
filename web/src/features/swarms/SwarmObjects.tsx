import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { accessApi, type AccessSwarm } from '@/services/brood/access';
import { actorsApi, templatesApi } from '@/services/brood/substrate';
import { SwarmAccess } from '@/features/access/SwarmAccess';
import { ObjectLink } from '@/features/objects/ObjectLink';
import { swarmCounts } from '@/features/objects/relations';
import { useResource } from '@/features/substrate/hooks';
import local from './SwarmPage.module.scss';

/** Список, отказ которого -- неизвестное число, а не отказ страницы. */
const orNull = <T,>(list: Promise<T>): Promise<T | null> => list.catch(() => null);

/** Акторы, шаблоны и участники роя одним чтением. */
async function loadObjects(id: string) {
  const [actors, templates, members] = await Promise.all([
    orNull(actorsApi.list(id)),
    orNull(templatesApi.list(id)),
    orNull(accessApi.swarmMembers(id)),
  ]);
  return { actors, templates, members };
}

function Count({ value }: { value: number | null }) {
  return value === null ? null : <span className={local.objectCount}>{value}</span>;
}

/** Объекты роя на его странице (#195): акторы и шаблоны -- ссылками на
 * страницы с одной группой этого роя, доступ -- кнопкой его листа; рядом
 * число. Списки читаются при открытии и кнопкой обновления в шапке, тиком
 * страницы (#257) -- нет: число объектов -- не исполнение. Непрочитанный
 * список -- ссылка без числа, а не ноль. У неактивного роя акторов и
 * шаблонов страницы не показывают, и строки нет. */
export function SwarmObjects({
  swarm,
  changed,
}: {
  swarm: AccessSwarm;
  changed: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const active = swarm.state === 'active';
  const load = useCallback(() => loadObjects(swarm.id), [swarm.id]);
  const objects = useResource(active ? load : null);
  const [access, setAccess] = useState(false);
  if (!active) return null;
  const counts = swarmCounts(objects.data ?? { actors: null, templates: null, members: null });
  const scope = { project: swarm.project_id, swarm: swarm.id };
  return (
    <>
      <nav
        className={local.objects}
        aria-label={t('swarm_page.objects.label')}
        data-swarm-objects=""
      >
        <ul className={local.metaList}>
          <li>
            <ObjectLink object={{ kind: 'actor', ...scope }}>
              {t('swarm_page.objects.actors')}
              <Count value={counts.actors} />
            </ObjectLink>
          </li>
          <li>
            <ObjectLink object={{ kind: 'template', ...scope }}>
              {t('swarm_page.objects.templates')}
              <Count value={counts.templates} />
            </ObjectLink>
          </li>
          <li>
            <button type="button" className={local.objectButton} onClick={() => setAccess(true)}>
              {t('swarm_page.objects.access')}
              <Count value={counts.members} />
            </button>
          </li>
        </ul>
      </nav>
      {access && (
        <SwarmAccess
          key={swarm.id}
          swarm={swarm}
          close={() => setAccess(false)}
          changed={async () => {
            await Promise.all([changed(), objects.refresh()]);
          }}
        />
      )}
    </>
  );
}
