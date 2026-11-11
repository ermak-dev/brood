/**
 * Легенда графа (#254): что из нарисованного стоит объяснить словами.
 * Чистая функция над разобранным деревом SVG (svgFilter): цвет и штрих
 * узла -- не единственный носитель смысла, легенда называет их.
 */
import type { ReactNode } from 'react';
import { svgNodeID, svgNodeKind } from './svgFilter';

/** Виды узлов со своим контуром в pipelines.module.scss; остальные
 * рисуются одинаково, объяснять их нечем. */
const STROKED_KINDS = ['actor', 'human', 'exit'] as const;

/** Метки состояний в порядке легенды: от ещё не начатого к неясному. */
const STATE_MARKS = ['pending', 'active', 'done', 'failed', 'skipped', 'unconfirmed'] as const;

export type LegendKind = (typeof STROKED_KINDS)[number];
export type LegendState = (typeof STATE_MARKS)[number];

export type LegendEntry =
  | { group: 'kind'; id: LegendKind }
  | { group: 'state'; id: LegendState };

/** Класс state-<x> движка -> метка, которой его красит модуль стилей
 * (те же пары, что в pipelines.module.scss). Незнакомое состояние не
 * выдаётся за другое и в легенду не попадает. */
const STATE_OF_CLASS: Record<string, LegendState> = {
  pending: 'pending',
  active: 'active',
  done: 'done',
  completed: 'done',
  failed: 'failed',
  refused: 'failed',
  skipped: 'skipped',
  stopped: 'skipped',
  unconfirmed: 'unconfirmed',
};

const propsOf = (node: ReactNode): Record<string, unknown> =>
  node !== null && typeof node === 'object' && 'props' in node
    ? ((node.props ?? {}) as Record<string, unknown>)
    : {};

const childrenOf = (node: ReactNode): ReactNode[] => {
  const children = propsOf(node).children;
  if (Array.isArray(children)) return children as ReactNode[];
  return children === undefined || children === null ? [] : [children as ReactNode];
};

/** Класс состояния узла: `state-<x>` или пустая строка. */
const stateClass = (node: ReactNode): string => {
  const cls = propsOf(node).className;
  const match = typeof cls === 'string' ? cls.match(/(?:^|\s)state-([-\w]+)(?:\s|$)/) : null;
  return match ? match[1] : '';
};

interface Seen {
  kinds: Set<string>;
  states: Set<LegendState>;
  unmarked: boolean;
  marked: boolean;
}

function walk(node: ReactNode, seen: Seen): void {
  if (svgNodeID(node) !== '') {
    seen.kinds.add(svgNodeKind(node));
    const state = STATE_OF_CLASS[stateClass(node)];
    if (state) {
      seen.states.add(state);
      seen.marked = true;
    } else if (stateClass(node) === '') {
      seen.unmarked = true;
    }
  }
  for (const child of childrenOf(node)) walk(child, seen);
}

/** Записи легенды для графа: виды узлов с особым контуром, когда их
 * больше одного, и состояния, которые проход уже раздал; pending -- для
 * узлов, до которых проход не дошёл. Пусто, когда объяснять нечего. */
export function legendEntries(root: ReactNode): LegendEntry[] {
  const seen: Seen = { kinds: new Set(), states: new Set(), unmarked: false, marked: false };
  if (root !== undefined && root !== null) walk(root, seen);
  const kinds = STROKED_KINDS.filter((kind) => seen.kinds.has(kind));
  if (seen.marked && seen.unmarked) seen.states.add('pending');
  const states = STATE_MARKS.filter((state) => seen.states.has(state));
  return [
    ...(kinds.length > 1 ? kinds.map((id) => ({ group: 'kind', id }) as const) : []),
    ...states.map((id) => ({ group: 'state', id }) as const),
  ];
}
