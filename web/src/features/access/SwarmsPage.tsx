import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { Button } from '@/components/ui/Button';
import { accessApi, type AccessProject, type AccessSwarm } from '@/services/brood/access';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { swarmPath } from '@/features/swarms/routes';
import { AccessEditor, LoadState, useAccessActions, useAccessResource } from './shared';
import { ProjectScope } from '@/features/substrate/components/ProjectScope';
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

/** Строка ведёт на страницу роя (#215): там его работа, а доступ, передача
 * и удаление — в конце страницы. Здесь остаётся создание роя. */
function Swarms({ project }: { project: AccessProject }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const projectId = project.id;
  const load = useCallback(() => accessApi.swarms(projectId), [projectId]);
  const resource = useAccessResource(load);
  const { run, busy } = useAccessActions(resource.refresh);
  const [creating, setCreating] = useState(false);
  // Имя вместо ID: полный ID с копированием — на странице роя.
  const columns: Column<AccessSwarm>[] = [
    { key: 'name', label: t('substrate.name'), primary: true, render: (swarm) => swarm.name },
    {
      key: 'state',
      label: t('access.state'),
      render: (swarm) => t(`access.states.${swarm.state}`, { defaultValue: swarm.state }),
    },
    {
      key: 'role',
      label: t('access.role'),
      render: (swarm) => t(`access.swarm_roles.${swarm.role}`),
    },
    {
      key: 'owner',
      label: t('access.owner'),
      render: (swarm) => swarm.owner_username,
    },
  ];
  return (
    <div className={styles.page}>
      <PageHeader
        title={t('access.swarms')}
        meta={[project.name]}
        actions={
          <Button disabled={busy} onClick={() => setCreating(true)}>
            {t('access.create_swarm')}
          </Button>
        }
      />
      <p className={styles.sectionHint}>{t('access.swarm_access_hint')}</p>
      <p className={styles.sectionHint}>{t('access.retry_hint')}</p>
      <LoadState {...resource} retry={resource.refresh}>
        <EntityTable
          columns={columns}
          rows={resource.data ?? []}
          rowKey={(swarm) => swarm.id}
          onRowClick={(swarm) => navigate(swarmPath(swarm.id))}
        />
        {resource.data?.length === 0 && <p>{t('access.no_swarms')}</p>}
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
