/**
 * Разбор и фильтрация серверного SVG для инлайнового графа (#214).
 *
 * Разметка приходит с сервера, но панель не исполняет её: строка
 * разбирается DOMParser'ом и переводится в React-элементы через
 * разрешающий словарь — неизвестные элементы и атрибуты выбрасываются.
 * Скрипты, foreignObject, обработчики событий и ссылки (href/xlink:href)
 * не проходят словарь в принципе. Элемент <style> и presentation-атрибуты
 * краски (fill, stroke) отбрасываются: серверная таблица стилей глобальна
 * в документе, а url() во внешний ресурс — запрос из страницы. Раскраску
 * даёт CSS-модуль панели по классам узлов (pipelines.module.scss).
 * Фильтр — защита границы, а не формальность.
 */

import { createElement, type ReactNode } from 'react';

/** Элементы SVG-словаря piprender. */
export const SVG_ELEMENT_ALLOWLIST = new Set([
  'svg',
  'g',
  'rect',
  'text',
  'polyline',
  'polygon',
  'path',
]);

/** Атрибуты словаря: обработчики и ссылки в него не входят. */
export const SVG_ATTRIBUTE_ALLOWLIST = new Set([
  'class',
  'viewBox',
  'x',
  'y',
  'x1',
  'y1',
  'x2',
  'y2',
  'width',
  'height',
  'rx',
  'ry',
  'points',
  'd',
  'cx',
  'cy',
  'r',
  'text-anchor',
  'font-size',
  'transform',
  'data-node-id',
]);

const isElement = (node: Node | null): node is Element =>
  !!node && node.nodeType === 1; /* ELEMENT_NODE */

/** Локальное имя без пространства имён: svg, g, rect, text, … */
const localName = (node: Node): string =>
  (node as Element).localName ?? node.nodeName.toLowerCase();

/** Значение атрибута не должно ссылаться наружу или исполняться. */
const safeAttributeValue = (value: string): boolean =>
  !/url\s*\(|javascript:|data:|https?:|\/\//i.test(value);

/** Дефисные атрибуты SVG в React-пропы: иначе React предупреждает
 * о неизвестном свойстве, хотя разметка та же. */
const ATTRIBUTE_PROPS = new Map([
  ['class', 'className'],
  ['text-anchor', 'textAnchor'],
  ['font-size', 'fontSize'],
]);

/** Классы словаря движка: чужой класс мог бы зацепить глобальные стили
 * панели, поэтому остальное отбрасывается. */
const SAFE_CLASS = /^(node|node-[-\w]+|state-[-\w]+|edge|edge-[-\w]+|edge-arrow|edge-label)$/;

/** Атрибут в React-проп: className вместо class, дефисы — в camelCase. */
const attributeProps = (element: Element): Record<string, string> => {
  const props: Record<string, string> = {};
  for (const attr of Array.from(element.attributes)) {
    const name = attr.name;
    if (name.startsWith('on') || !SVG_ATTRIBUTE_ALLOWLIST.has(name)) continue;
    // ID узла — идентификатор, не ссылка: проверяются остальные значения.
    if (name !== 'data-node-id' && !safeAttributeValue(attr.value)) continue;
    if (name === 'class') {
      const classes = attr.value.split(/\s+/).filter((c) => SAFE_CLASS.test(c));
      if (classes.length > 0) props.className = classes.join(' ');
      continue;
    }
    props[ATTRIBUTE_PROPS.get(name) ?? name] = attr.value;
  }
  return props;
};

/** Переводит разобранный DOM-узел в React-элемент словаря. Текстовые
 * узлы проходят как есть: подписи рёбер и узлов — текст. */
const toReactNode = (node: Node, key: string): ReactNode => {
  if (node.nodeType === 3 /* TEXT_NODE */) {
    return node.nodeValue ?? '';
  }
  if (!isElement(node)) return null;
  const name = localName(node);
  if (!SVG_ELEMENT_ALLOWLIST.has(name)) return null;
  const props = { ...attributeProps(node), key };
  const children = Array.from(node.childNodes)
    .map((child, index) => toReactNode(child, `${key}.${index}`))
    .filter((child): child is ReactNode => child !== null);
  return createElement(name, props, ...children);
};

interface DOMImplementationLike {
  createHTMLDocument: (title?: string) => Document;
}

/**
 * Разбирает серверный SVG и возвращает отфильтрованные React-элементы.
 * Корень обязан быть svg: любой другой документ даёт пустой список.
 */
export function parseServerSVG(source: string, dom?: DOMImplementationLike): ReactNode[] {
  const impl = dom ?? (typeof document !== 'undefined' ? document.implementation : undefined);
  if (!impl) return [];
  const parsed = new DOMParser().parseFromString(source, 'image/svg+xml');
  if (parsed.getElementsByTagName('parsererror').length > 0) return [];
  const root = parsed.documentElement;
  if (!root || localName(root) !== 'svg') return [];
  return [toReactNode(root, 'svg')].filter((node): node is ReactNode => node !== null);
}

/** Собственный размер графа из viewBox: граф рисуется в масштабе 1:1, а
 * не растягивается на контейнер, поэтому широкий граф прокручивается.
 * Неразборчивый или неположительный размер — undefined. */
export function svgViewBoxSize(viewBox: unknown): { width: number; height: number } | undefined {
  if (typeof viewBox !== 'string') return undefined;
  const parts = viewBox.trim().split(/[\s,]+/).map(Number);
  if (parts.length !== 4 || parts.some((n) => !Number.isFinite(n))) return undefined;
  const [, , width, height] = parts;
  return width > 0 && height > 0 ? { width, height } : undefined;
}

/** ID узла из data-node-id; пустая строка, если атрибута нет. */
export function svgNodeID(node: ReactNode): string {
  if (node && typeof node === 'object' && 'props' in node) {
    const id = (node.props as Record<string, unknown>)['data-node-id'];
    if (typeof id === 'string') return id;
  }
  return '';
}

const nodeClasses = (node: ReactNode): string => {
  if (node && typeof node === 'object' && 'props' in node) {
    const cls = (node.props as Record<string, unknown>).className;
    if (typeof cls === 'string') return cls;
  }
  return '';
};

/** Вид узла из класса node-<kind>; пустая строка, если нет. */
export function svgNodeKind(node: ReactNode): string {
  const match = nodeClasses(node).match(/(?:^|\s)node-([-\w]+)(?:\s|$)/);
  return match ? match[1] : '';
}

/** Состояние узла из класса state-<state>; пустая строка, если нет. */
export function svgNodeState(node: ReactNode): string {
  const match = nodeClasses(node).match(/(?:^|\s)state-([-\w]+)(?:\s|$)/);
  return match ? match[1] : '';
}

const firstText = (node: ReactNode): string => {
  if (node === null || node === undefined || typeof node === 'boolean') return '';
  if (typeof node === 'string' || typeof node === 'number') return String(node);
  if (Array.isArray(node)) return node.map(firstText).join('');
  if (typeof node === 'object' && 'props' in node) {
    const props = (node as { props?: { children?: unknown } }).props;
    return firstText(props?.children as ReactNode);
  }
  return '';
};

/** Подпись узла: текст его <text>-элемента. */
export function svgNodeLabel(node: ReactNode): string {
  if (node && typeof node === 'object' && 'props' in node) {
    const children = (node.props as { children?: unknown }).children;
    const list = Array.isArray(children) ? children : [children];
    for (const child of list) {
      if (
        child &&
        typeof child === 'object' &&
        'type' in child &&
        (child as { type?: unknown }).type === 'text'
      ) {
        return firstText(child as ReactNode);
      }
    }
  }
  return '';
}
