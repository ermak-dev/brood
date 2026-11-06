import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { useInterval } from '@/hooks/useInterval';
import { pipelinesApi, type RunNode } from '@/services/brood/pipelines';
import { LoadState, StatusBadge } from '@/features/access/shared';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import { Section } from '@/features/substrate/components/form';
import { useResource } from '@/features/substrate/hooks';
import { gateVisitKey, openGate, runIsLive, runTone } from './state';
import { HumanGatePanel } from './HumanGatePanel';
import styles from '@/features/substrate/substrate.module.scss';
import graphStyles from './pipelines.module.scss';

export function RunGraph({ svg }: { svg: string }) {
  const { t } = useTranslation();
  return (
    <div className={graphStyles.graph}>
      {/* SVG может содержать пользовательский ввод; в контексте изображения
          браузер не исполняет скрипты и не загружает внешние ресурсы. */}
      <img src={`data:image/svg+xml,${encodeURIComponent(svg)}`} alt={t('pipelines.graph_label')} />
    </div>
  );
}

export function NodeTimeline({ nodes }: { nodes: RunNode[] }) {
  const { t } = useTranslation();
  return (
    <ul className={graphStyles.timeline}>
      {nodes.map((node, index) => (
        <li key={`${node.id}-${index}`} className={graphStyles['timeline-node']}>
          <span className={graphStyles[`timeline-${node.status}`]} aria-hidden="true" />
          <span className={graphStyles['timeline-label']}>
            {node.label}
            <span className={graphStyles['timeline-kind']}>
              {' · '}
              {t(`pipelines.kind.${node.kind}`)}
            </span>
          </span>
          {node.status === 'unconfirmed' && (
            <span className={graphStyles['timeline-outcome']}>{t('actors.ops.unconfirmed_badge')}</span>
          )}
          {node.outcome && (
            <span className={graphStyles['timeline-outcome']}>{node.outcome}</span>
          )}
        </li>
      ))}
    </ul>
  );
}

/** Один проход (#169): состояние, граф движка и узлы по шагам. Ответ
 * на вопрос уходит answerGate и перечитывает проход. */
export function RunDetailPage() {
  const { t, i18n } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const loadRun = useCallback(() => pipelinesApi.getRun(id ?? ''), [id]);
  const run = useResource(id ? loadRun : null);
  const loadGraph = useCallback(() => pipelinesApi.getRunGraph(id ?? ''), [id]);
  const graph = useResource(id ? loadGraph : null, { headerRefresh: false });
  const detail = run.data;
  const gate = detail ? openGate(detail) : null;
  useInterval(
    () => {
      void run.refresh();
      void graph.refresh();
    },
    id && runIsLive(detail) ? 5000 : null
  );

  if (!id) return null;
  return (
    <div className={styles.page}>
      <PageHeader
        title={detail?.name ?? id}
        meta={[
          <Link key="back" to="/pipelines">
            {t('pipelines.title')}
          </Link>,
          detail?.startedAt && new Date(detail.startedAt).toLocaleString(i18n.language),
          detail?.finishedAt && new Date(detail.finishedAt).toLocaleString(i18n.language),
        ]}
      />
      <LoadState loading={run.loading} error={run.error ?? ''} retry={run.refresh}>
        {detail && (
          <>
            <p>
              <StatusBadge tone={runTone(detail.status)}>
                {t(`pipelines.statuses.${detail.status}`)}
              </StatusBadge>
            </p>
            {graph.data?.svg && <RunGraph svg={graph.data.svg} />}
            {gate && (
              <Section title={t('pipelines.gate_title')}>
                <HumanGatePanel
                  key={gateVisitKey(gate)}
                  gate={gate}
                  submit={async (answer) => {
                    await pipelinesApi.answerGate(detail.id, gate.nodeId, answer, gate.visitSequence);
                    await Promise.all([run.refresh(), graph.refresh()]);
                  }}
                />
              </Section>
            )}
            <Section title={t('pipelines.timeline')}>
              <NodeTimeline nodes={detail.nodes} />
            </Section>
          </>
        )}
      </LoadState>
    </div>
  );
}
