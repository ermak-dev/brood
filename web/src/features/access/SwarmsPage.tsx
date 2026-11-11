import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { accessApi, type AccessProject, type AccessSwarm } from '@/services/brood/access';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { DASH } from '@/features/substrate/format';
import { swarmPath } from '@/features/swarms/routes';
import { useSwarmActivityStore } from '@/stores';
import { AccessEditor, LoadState, StatusBadge, useAccessActions, useAccessResource } from './shared';
import { ProjectScope } from '@/features/substrate/components/ProjectScope';
import { swarmTone } from './state';
import {
  ACTIVITY_LIMIT,
  activityTargets,
  executionActivityTone,
  swarmActivityOf,
  swarmOrder,
  type SwarmActivity,
} from './swarmActivity';
import styles from '@/features/substrate/substrate.module.scss';

/** Рои глобально выбранного проекта; каталог уже отфильтрован сервером. */
export function SwarmsPage() {
  const { t } = useTranslation();
  return (
    <ProjectScope title={t('access.swarms')}>
      {(project) => <Swarms project={project} />}
    </ProjectScope>
  );
}

/** Ещё не прочитанная сводка: «…», пока опрос идёт. */
const PENDING = '…';

type Activities = Record<string, SwarmActivity | null | undefined>;

/** Что показать в колонках исполнения: сводка сервера или собранная;
 * неактивный рой, рой за пределом опроса и отказ опроса -- прочерк. */
function activityCell(swarm: AccessSwarm, activities: Activities, polled: Set<string>) {
  const known = swarmActivityOf(swarm);
  if (known) return known;
  if (!polled.has(swarm.id)) return null;
  return activities[swarm.id];
}

/** Таблица роёв (#255): инфраструктура и исполнение -- отдельные колонки,
 * ждущие ответа сверху. Строка ведёт на страницу роя (#215). */
export function SwarmsTable({
  swarms,
  activities,
}: {
  swarms: AccessSwarm[];
  activities: Activities;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const polled = new Set(activityTargets(swarms).map((swarm) => swarm.id));
  const rows = [...swarms].sort(swarmOrder(activities));
  // Имя вместо ID: полный ID с копированием -- на странице роя.
  const columns: Column<AccessSwarm>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (swarm) => swarm.name },
    {
      key: 'state',
      label: t('access.state'),
      render: (swarm) => (
        <StatusBadge tone={swarmTone(swarm.state)}>
          {t(`access.states.${swarm.state}`, { defaultValue: swarm.state })}
        </StatusBadge>
      ),
    },
    {
      key: 'execution',
      label: t('access.execution'),
      render: (swarm) => {
        const activity = activityCell(swarm, activities, polled);
        if (activity === undefined) return PENDING;
        if (activity === null) return DASH;
        return (
          <StatusBadge tone={executionActivityTone(activity.execution)}>
            {t(`access.execution_states.${activity.execution}`, {
              defaultValue: activity.execution,
            })}
          </StatusBadge>
        );
      },
    },
    {
      key: 'requests',
      label: t('access.open_requests'),
      render: (swarm) => {
        const activity = activityCell(swarm, activities, polled);
        if (activity === undefined) return PENDING;
        if (activity === null) return DASH;
        return activity.openRequests > 0 ? (
          <StatusBadge tone="attention">{activity.openRequests}</StatusBadge>
        ) : (
          String(activity.openRequests)
        );
      },
    },
    {
      key: 'role',
      label: t('access.role'),
      hideOnMobile: true,
      render: (swarm) => t(`access.swarm_roles.${swarm.role}`),
    },
    {
      key: 'owner',
      label: t('access.owner'),
      render: (swarm) => swarm.owner_username,
    },
  ];
  return (
    <EntityTable
      columns={columns}
      rows={rows}
      rowKey={(swarm) => swarm.id}
      onRowClick={(swarm) => navigate(swarmPath(swarm.id))}
    />
  );
}

/** Строка ведёт на страницу роя (#215): там его работа, а доступ, передача
 * и удаление -- в конце страницы. Здесь остаётся создание роя. Сводку
 * исполнения страница отдаёт общему хранилищу: меню считает по ней ждущих. */
function Swarms({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const projectId = project.id;
  const load = useCallback(() => accessApi.swarms(projectId), [projectId]);
  const resource = useAccessResource(load);
  const { run, busy } = useAccessActions(resource.refresh);
  const [creating, setCreating] = useState(false);
  const adopt = useSwarmActivityStore((state) => state.adopt);
  const activityProject = useSwarmActivityStore((state) => state.project);
  const loaded = useSwarmActivityStore((state) => state.activities);
  const activities = activityProject === projectId ? loaded : {};
  const swarms = resource.data;
  useEffect(() => {
    if (swarms) void adopt(projectId, swarms);
  }, [adopt, projectId, swarms]);
  const unpolled = swarms
    ? swarms.filter((swarm) => swarm.state === 'active' && !swarmActivityOf(swarm)).length -
      activityTargets(swarms).length
    : 0;
  // Подпись о повторе -- только когда в списке есть незаконченная операция.
  const retryable =
    swarms?.some((swarm) => ['provisioning', 'error', 'deleting'].includes(swarm.state)) ?? false;
  const create = (
    <Button disabled={busy} onClick={() => setCreating(true)}>
      {t('access.create_swarm')}
    </Button>
  );
  return (
    <div className={styles.page}>
      <PageHeader title={t('access.swarms')} meta={[project.name]} actions={create} />
      <LoadState {...resource} retry={resource.refresh}>
        {swarms && swarms.length > 0 ? (
          <>
            <SwarmsTable swarms={swarms} activities={activities} />
            {unpolled > 0 && (
              <p className={styles.sectionHint}>
                {t('access.activity_limited', { count: ACTIVITY_LIMIT })}
              </p>
            )}
            {retryable && <p className={styles.sectionHint}>{t('access.retry_hint')}</p>}
          </>
        ) : (
          <div className={styles.empty}>
            <EmptyState
              title={t('access.no_swarms')}
              description={t('access.swarm_access_hint')}
              action={create}
            />
          </div>
        )}
      </LoadState>
      {creating && (
        <AccessEditor
          key={projectId}
          title={t('access.create_swarm')}
          close={() => setCreating(false)}
          fields={[{ name: 'name', label: t('substrate.name'), required: true }]}
          submit={(values) => run(() => accessApi.createSwarm(projectId, values.name.trim()))}
        />
      )}
    </div>
  );
}
