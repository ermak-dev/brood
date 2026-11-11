import { createElement, useId, useMemo, type ReactNode, type KeyboardEvent } from 'react';
import { useTranslation } from 'react-i18next';
import {
  parseServerSVG,
  svgNodeID,
  svgNodeKind,
  svgNodeLabel,
  svgNodeState,
  svgViewBoxSize,
} from './svgFilter';
import graphStyles from './pipelines.module.scss';
import styles from '@/features/substrate/substrate.module.scss';

/** Инлайновый граф (#214): серверный SVG — часть документа. Разметка
 * проходит разрешающий фильтр (svgFilter), не innerHTML. Узлы — кнопки
 * только когда есть onSelect: без обработчика они просто текст графа и
 * не попадают в порядок табуляции. Активный узел (где стоит исполнение) и
 * выбранный (куда смотрит оператор) — разные состояния: первый несёт
 * aria-current, второй aria-pressed и свой контур. Рисует граф роя на его
 * странице и граф определения в диалоге запуска (#217). */
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
  const descriptionId = useId();
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
        aria-describedby={descriptionId}
        className={graphStyles.svg}
      >
        {childrenOf(root).map((child, index) => decorate(child, `n${index}`, ctx))}
      </svg>
      {/* Что такое узлы и как читать граф без цвета (#256): скринридеру,
       * зрячему состояние говорят заливка и имя узла. */}
      <p id={descriptionId} className={styles.srOnly}>
        {t('pipelines.graph_description')}
      </p>
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
  // Состояние — часть имени («review, actor, Done»): иначе его несёт только
  // цвет. Словарь тот же, что у истории шагов; неизвестное состояние, как и
  // вид, подписывается как есть.
  const state = svgNodeState(node);
  const stateName =
    state === '' ? '' : ctx.t(`swarm_page.node_states.${state}`, { defaultValue: state });
  const nodeLabel = svgNodeLabel(node);
  return createElement(
    'g',
    {
      ...common,
      children: undefined,
      tabIndex: 0,
      role: 'button',
      'aria-pressed': selected,
      'aria-label': [nodeLabel, kindName, stateName].filter((part) => part !== '').join(', '),
      onClick: () => onSelect(id),
      onKeyDown: (event: KeyboardEvent) => nodeKeyDown(event, () => onSelect(id)),
    },
    ...next
  );
}
