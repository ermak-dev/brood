import { useCallback, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { LoadState } from '@/features/access/shared';
import { Section } from '@/features/substrate/components/form';
import { useResource } from '@/features/substrate/hooks';
import { pipelinesApi, type WorkflowDefinition } from '@/services/brood/pipelines';
import { RunGraph } from './RunDetailPage';
import { selectedGraphKey } from './catalogState';

type CatalogProps = {
  definitions: WorkflowDefinition[] | null;
  loading: boolean;
  error: string;
  retry: () => Promise<void>;
  select: (name: string) => void;
  selected?: string;
};

export function WorkflowCatalogView({
  definitions,
  loading,
  error,
  retry,
  select,
  selected,
}: CatalogProps) {
  const { t, i18n } = useTranslation();
  return (
    <Section title={t('pipelines.definitions')}>
      <LoadState loading={loading} error={error} retry={retry}>
        {definitions?.length === 0 && <p>{t('pipelines.definitions_empty')}</p>}
        {definitions && definitions.length > 0 && (
          <ul>
            {definitions.map((definition) => (
              <li key={definition.name}>
                <Button
                  type="button"
                  variant="secondary"
                  aria-pressed={selected === definition.name}
                  onClick={() => select(definition.name)}
                >
                  {definition.name}
                </Button>{' '}
                {t('pipelines.definition_version', { version: definition.version })}
                {definition.publishedAt &&
                  ` · ${new Date(definition.publishedAt).toLocaleString(i18n.language)}`}
              </li>
            ))}
          </ul>
        )}
      </LoadState>
    </Section>
  );
}

function DefinitionGraph({
  projectId,
  definition,
  refreshCatalog,
}: {
  projectId: string;
  definition: WorkflowDefinition;
  refreshCatalog: () => Promise<void>;
}) {
  const { t } = useTranslation();
  const { name, version, revision } = definition;
  const load = useCallback(
    () => pipelinesApi.getDefinitionGraph(projectId, name, version, revision),
    [projectId, name, version, revision]
  );
  const graph = useResource(load, { headerRefresh: false });
  return (
    <Section title={t('pipelines.definition_graph', { name })}>
      <LoadState loading={graph.loading} error={graph.error ?? ''} retry={refreshCatalog}>
        {graph.data?.svg ? (
          <RunGraph svg={graph.data.svg} label={t('pipelines.definition_graph', { name })} />
        ) : (
          <p>{t('pipelines.graph_empty')}</p>
        )}
      </LoadState>
    </Section>
  );
}

export function WorkflowCatalog({ projectId }: { projectId: string }) {
  return <ProjectWorkflowCatalog key={projectId} projectId={projectId} />;
}

function ProjectWorkflowCatalog({ projectId }: { projectId: string }) {
  const [selected, select] = useState('');
  const [graphAttempt, setGraphAttempt] = useState(0);
  const load = useCallback(() => pipelinesApi.listDefinitions(projectId), [projectId]);
  const catalog = useResource(load, { trackRefresh: true });
  const refreshCatalog = useCallback(async () => {
    await catalog.refresh();
    setGraphAttempt((attempt) => attempt + 1);
  }, [catalog.refresh]);
  const graphKey = selectedGraphKey(
    projectId,
    selected,
    catalog.data,
    catalog.loading,
    catalog.error ?? ''
  );
  const current = graphKey
    ? catalog.data?.find((definition) => definition.name === selected)
    : undefined;
  return (
    <>
      <WorkflowCatalogView
        definitions={catalog.data}
        loading={catalog.loading}
        error={catalog.error ?? ''}
        retry={catalog.refresh}
        select={select}
        selected={current?.name}
      />
      {graphKey && current && (
        <DefinitionGraph
          key={`${graphKey}:${graphAttempt}`}
          projectId={projectId}
          definition={current}
          refreshCatalog={refreshCatalog}
        />
      )}
    </>
  );
}
