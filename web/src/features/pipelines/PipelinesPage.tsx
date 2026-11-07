import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { useInterval } from '@/hooks/useInterval';
import { pipelinesApi, type PipelineRun } from '@/services/brood/pipelines';
import { EntityTable, type Column } from '@/features/substrate/components/EntityTable';
import { LoadState, StatusBadge } from '@/features/access/shared';
import { ProjectScope } from '@/features/substrate/components/ProjectScope';
import { SwarmSections } from '@/features/substrate/components/SwarmSections';
import { useResource } from '@/features/substrate/hooks';
import { DASH } from '@/features/substrate/format';
import { runTone, sortRuns } from './state';
import { workflowRunPath } from './routes';
import { WorkflowCatalog } from './WorkflowCatalog';
import styles from '@/features/substrate/substrate.module.scss';

/** Таблица проходов отдельным компонентом: страница грузит список, таблица
 * только рисует его (и так же тестируется статической разметкой). */
export function RunTable({
  runs,
  onOpen,
}: {
  runs: PipelineRun[];
  onOpen: (run: PipelineRun) => void;
}) {
  const { t, i18n } = useTranslation();
  const columns: Column<PipelineRun>[] = [
    { key: 'name', label: t('pipelines.name'), primary: true, render: (run) => run.name },
    {
      key: 'status',
      label: t('pipelines.status'),
      render: (run) => (
        <StatusBadge tone={runTone(run.status)}>
          {t(`pipelines.statuses.${run.status}`)}
        </StatusBadge>
      ),
    },
    {
      key: 'started',
      label: t('pipelines.started'),
      render: (run) =>
        run.startedAt ? new Date(run.startedAt).toLocaleString(i18n.language) : DASH,
    },
    {
      key: 'current',
      label: t('pipelines.current_node'),
      render: (run) => run.currentNode ?? DASH,
    },
  ];
  return <EntityTable columns={columns} rows={runs} rowKey={(run) => run.id} onRowClick={onOpen} />;
}

/** Проходы доступных роёв выбранного проекта обновляются независимо. */
export function PipelinesPage() {
  const { t } = useTranslation();
  return (
    <ProjectScope title={t('pipelines.title')}>
      {(project) => (
        <>
          <div className={styles.page}>
            <WorkflowCatalog key={project.id} projectId={project.id} />
          </div>
          <SwarmSections project={project} title={t('pipelines.runs')}>
            {(swarm) => <SwarmRuns key={swarm.id} swarmId={swarm.id} name={swarm.name} />}
          </SwarmSections>
        </>
      )}
    </ProjectScope>
  );
}

function SwarmRuns({ swarmId, name }: { swarmId: string; name: string }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const load = useCallback(() => pipelinesApi.listRuns(swarmId), [swarmId]);
  const { data, error, loading, refresh } = useResource(load);
  useInterval(
    () => {
      void refresh();
    },
    loading ? null : 5000
  );

  return (
    <div className={styles.page}>
      <h2>{name}</h2>
      <LoadState loading={loading} error={error ?? ''} retry={refresh}>
        <RunTable runs={sortRuns(data ?? [])} onOpen={(run) => navigate(workflowRunPath(run.id))} />
        {data?.length === 0 && <p>{t('pipelines.empty')}</p>}
      </LoadState>
    </div>
  );
}
