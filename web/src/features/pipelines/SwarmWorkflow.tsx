import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { LoadState } from '@/features/access/shared';
import { Section } from '@/features/substrate/components/form';
import { useAccessResource } from '@/features/access/shared';
import { pipelinesApi, type PipelineRun, type RunGraph as Graph } from '@/services/brood/pipelines';
import { RunGraph } from './RunDetailPage';
import { sortRuns } from './state';

type ViewProps = {
  runs: PipelineRun[] | null;
  loading: boolean;
  error: string;
  retry: () => Promise<void>;
  selectedId?: string;
  select?: (id: string) => void;
  graph?: Graph | null;
  graphLoading?: boolean;
  graphError?: string;
  retryGraph?: () => Promise<void>;
};

export function SwarmWorkflowView({
  runs,
  loading,
  error,
  retry,
  selectedId,
  select,
  graph,
  graphLoading,
  graphError,
  retryGraph,
}: ViewProps) {
  const { t } = useTranslation();
  const sorted = sortRuns(runs ?? []);
  const selected = sorted.find((run) => run.id === selectedId) ?? sorted[0];
  return (
    <Section title={t('pipelines.title')}>
      <LoadState loading={loading} error={error} retry={retry}>
        {sorted.length === 0 ? (
          <p>{t('pipelines.empty')}</p>
        ) : (
          selected && (
            <>
              <label>
                {t('pipelines.name')}{' '}
                <select value={selected.id} onChange={(event) => select?.(event.target.value)}>
                  {sorted.map((run) => (
                    <option key={run.id} value={run.id}>
                      {run.name} · {t(`pipelines.statuses.${run.status}`)} ·{' '}
                      {run.startedAt ?? run.id}
                    </option>
                  ))}
                </select>
              </label>
              <p>
                <Link to={`/pipelines/${encodeURIComponent(selected.id)}`}>{selected.name}</Link>
              </p>
              <LoadState
                loading={!!graphLoading}
                error={graphError ?? ''}
                retry={retryGraph ?? retry}
              >
                {graph?.svg ? <RunGraph svg={graph.svg} /> : <p>{t('pipelines.graph_empty')}</p>}
              </LoadState>
            </>
          )
        )}
      </LoadState>
    </Section>
  );
}

function SelectedGraph({
  runs,
  selectedId,
  select,
}: {
  runs: PipelineRun[];
  selectedId: string;
  select: (id: string) => void;
}) {
  // Новый снимок списка требует и новый граф, даже если React объединил состояния загрузки.
  const load = useCallback(() => {
    void runs;
    return pipelinesApi.getRunGraph(selectedId);
  }, [selectedId, runs]);
  const graph = useAccessResource(load, { headerRefresh: false });
  return (
    <SwarmWorkflowView
      runs={runs}
      loading={false}
      error=""
      retry={graph.refresh}
      selectedId={selectedId}
      select={select}
      graph={graph.data}
      graphLoading={graph.loading}
      graphError={graph.error}
      retryGraph={graph.refresh}
    />
  );
}

export function SwarmWorkflow({ swarmId }: { swarmId: string }) {
  const [choice, select] = useState('');
  const load = useCallback(() => pipelinesApi.listRuns(swarmId), [swarmId]);
  const runs = useAccessResource(load);
  const sorted = sortRuns(runs.data ?? []);
  const selected = sorted.find((run) => run.id === choice) ?? sorted[0];
  if (!selected)
    return (
      <SwarmWorkflowView
        runs={runs.data}
        loading={runs.loading}
        error={runs.error}
        retry={runs.refresh}
      />
    );
  return <SelectedGraph runs={runs.data ?? []} selectedId={selected.id} select={select} />;
}
