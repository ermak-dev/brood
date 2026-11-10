import {
  createElement,
  useCallback,
  useMemo,
  useState,
  type ReactNode,
  type KeyboardEvent,
} from 'react';
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
import { WORKFLOWS_PATH } from './routes';
import {
  parseServerSVG,
  svgNodeID,
  svgNodeKind,
  svgNodeLabel,
  svgViewBoxSize,
} from './svgFilter';
import styles from '@/features/substrate/substrate.module.scss';
import graphStyles from './pipelines.module.scss';

/** Инлайновый граф (#214): серверный SVG — часть документа. Разметка
 * проходит разрешающий фильтр (svgFilter), не innerHTML. Узлы — кнопки
 * только когда есть onSelect: без обработчика они просто текст графа и
 * не попадают в порядок табуляции. Активный узел (где стоит проход) и
 * выбранный (куда смотрит оператор) — разные состояния: первый несёт
 * aria-current, второй aria-pressed и свой контур. */
export function RunGraph({
  svg,
  label,
  activeNode,
  selectedNode,
  onSelect,
}: {
  svg: string;
  label?: string;
  activeNode?: string;
  selectedNode?: string;
  onSelect?: (id: string) => void;
}) {
  const { t } = useTranslation();
  // Разбор и фильтрация дороги: один раз на строку SVG.
  const root = useMemo(() => parseServerSVG(svg)[0], [svg]);
  const known = useMemo(() => collectNodeIDs(root), [root]);
  // Выбор узла, которого нет в этом графе, не подсвечивается: так выбор
  // не переживает смену графа.
  const selected = selectedNode !== undefined && known.has(selectedNode) ? selectedNode : undefined;
  if (!root || typeof root !== 'object' || !('props' in root)) {
    return <div className={graphStyles.graph} />;
  }
  const props = root.props as { viewBox?: unknown };
  const size = svgViewBoxSize(props.viewBox);
  const ctx: Decorate = { t, activeNode, selectedNode: selected, onSelect };
  return (
    <div className={graphStyles.graph}>
      {/* Не role="img": дети-кнопки скрылись бы от скринридера. */}
      <svg
        xmlns="http://www.w3.org/2000/svg"
        viewBox={typeof props.viewBox === 'string' ? props.viewBox : undefined}
        width={size?.width}
        height={size?.height}
        role="group"
        aria-label={label ?? t('pipelines.graph_label')}
        className={graphStyles.svg}
      >
        {childrenOf(root).map((child, index) => decorate(child, `n${index}`, ctx))}
      </svg>
    </div>
  );
}

type Decorate = {
  t: (key: string, options?: { defaultValue?: string }) => string;
  activeNode?: string;
  selectedNode?: string;
  onSelect?: (id: string) => void;
};

const propsOf = (node: ReactNode): Record<string, unknown> =>
  node !== null && typeof node === 'object' && 'props' in node
    ? ((node.props ?? {}) as Record<string, unknown>)
    : {};

function childrenOf(node: ReactNode): ReactNode[] {
  const children = propsOf(node).children;
  if (Array.isArray(children)) return children as ReactNode[];
  return children === undefined || children === null ? [] : [children as ReactNode];
}

/** ID всех узлов графа. */
function collectNodeIDs(node: ReactNode, into = new Set<string>()): Set<string> {
  const id = svgNodeID(node);
  if (id !== '') into.add(id);
  for (const child of childrenOf(node)) collectNodeIDs(child, into);
  return into;
}

/** Enter и Space выбирают узел, как кнопку; Space не прокручивает страницу. */
function nodeKeyDown(event: KeyboardEvent, select: () => void) {
  if (event.key !== 'Enter' && event.key !== ' ') return;
  event.preventDefault();
  select();
}

function decorate(node: ReactNode, key: string, ctx: Decorate): ReactNode {
  const type = node !== null && typeof node === 'object' && 'type' in node ? node.type : undefined;
  if (typeof type !== 'string') return node;
  const props = propsOf(node);
  const next = childrenOf(node).map((child, index) => decorate(child, `${key}.${index}`, ctx));
  const id = svgNodeID(node);
  if (id === '') {
    return createElement(type, { ...props, children: undefined, key }, ...next);
  }
  const selected = id === ctx.selectedNode;
  const common = {
    ...props,
    key,
    'aria-current': id === ctx.activeNode ? ('true' as const) : undefined,
  };
  if (!ctx.onSelect) {
    return createElement('g', { ...common, children: undefined }, ...next);
  }
  const onSelect = ctx.onSelect;
  const kind = svgNodeKind(node);
  // Неизвестный вид (сервер новее панели) подписывается как есть.
  const kindName = kind === '' ? '' : ctx.t(`pipelines.kind.${kind}`, { defaultValue: kind });
  const nodeLabel = svgNodeLabel(node);
  return createElement(
    'g',
    {
      ...common,
      children: undefined,
      tabIndex: 0,
      role: 'button',
      'aria-pressed': selected,
      'aria-label': kindName === '' ? nodeLabel : `${nodeLabel}, ${kindName}`,
      onClick: () => onSelect(id),
      onKeyDown: (event: KeyboardEvent) => nodeKeyDown(event, () => onSelect(id)),
    },
    ...next
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
            <span className={graphStyles['timeline-outcome']}>
              {t('actors.ops.unconfirmed_badge')}
            </span>
          )}
          {node.outcome && <span className={graphStyles['timeline-outcome']}>{node.outcome}</span>}
        </li>
      ))}
    </ul>
  );
}

/** Один проход (#169): состояние, граф движка и узлы по шагам. Ответ
 * на вопрос уходит answerGate и перечитывает проход. Выбор узла (#214) —
 * состояние страницы: активный узел подсвечен, выбранный управляет
 * остальной частью страницы. */
export function RunDetailPage() {
  const { t, i18n } = useTranslation();
  const { id } = useParams<{ id: string }>();
  // Выбор принадлежит проходу: чужой проход его не наследует.
  const [selection, setSelection] = useState<{ runId: string; node: string } | undefined>();
  const selectedNode = selection && selection.runId === id ? selection.node : undefined;
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
          <Link key="back" to={WORKFLOWS_PATH}>
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
            {graph.data?.svg && (
              <RunGraph
                svg={graph.data.svg}
                activeNode={detail.currentNode}
                selectedNode={selectedNode}
                onSelect={(node) => setSelection({ runId: id ?? '', node })}
              />
            )}
            {gate && (
              <Section title={t('pipelines.gate_title')}>
                <HumanGatePanel
                  key={gateVisitKey(gate)}
                  gate={gate}
                  submit={async (answer) => {
                    await pipelinesApi.answerGate(
                      detail.id,
                      gate.nodeId,
                      answer,
                      gate.visitSequence
                    );
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
