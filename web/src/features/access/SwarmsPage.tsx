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
import { executionActivityTone, swarmActivityOf, swarmOrder } from './swarmActivity';
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

/** Таблица роёв (#255): инфраструктура и исполнение -- отдельные колонки,
 * ждущие ответа сверху. Сводку исполнения несёт строка списка (#234); без
 * неё -- прочерк. Строка ведёт на страницу роя (#215). */
export function SwarmsTable({ swarms }: { swarms: AccessSwarm[] }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const rows = [...swarms].sort(swarmOrder);
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
        const activity = swarmActivityOf(swarm);
        if (!activity) return DASH;
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
        const activity = swarmActivityOf(swarm);
        if (!activity) return DASH;
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
 * и удаление -- в конце страницы. Здесь остаётся создание роя. Прочитанный
 * список страница отдаёт общему хранилищу: меню считает по нему ждущих. */
function Swarms({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const projectId = project.id;
  const load = useCallback(() => accessApi.swarms(projectId), [projectId]);
  const resource = useAccessResource(load);
  const { run, busy } = useAccessActions(resource.refresh);
  const [creating, setCreating] = useState(false);
  const adopt = useSwarmActivityStore((state) => state.adopt);
  const swarms = resource.data;
  useEffect(() => {
    if (swarms) adopt(projectId, swarms);
  }, [adopt, projectId, swarms]);
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
            <SwarmsTable swarms={swarms} />
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
