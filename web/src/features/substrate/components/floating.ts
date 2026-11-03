import { useEffect, useRef, type CSSProperties } from 'react';

const MARGIN = 8;
const OFFSET = 6;
/** Выше Sheet (2000) и Select (2010): меню и подсказки открываются и в деталях. */
export const FLOATING_Z = 2100;

/** Позиция всплывающего слоя у якоря: fixed, по правому краю якоря, вниз или
 * вверх — где больше места; не выходит за экран. Таблица прокручивается
 * внутри overflow-контейнера, поэтому слой живёт в портале, а не в ячейке. */
export function floatingStyle(
  anchor: DOMRect,
  size: { width: number; height: number },
  align: 'end' | 'center' = 'end'
): CSSProperties {
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const width = Math.min(size.width, vw - MARGIN * 2);
  const preferred = align === 'end' ? anchor.right - width : anchor.left + (anchor.width - width) / 2;
  const left = Math.min(Math.max(preferred, MARGIN), vw - width - MARGIN);
  const below = vh - anchor.bottom - OFFSET - MARGIN;
  const above = anchor.top - OFFSET - MARGIN;
  const down = below >= size.height || below >= above;
  const maxHeight = Math.max(120, down ? below : above);
  return down
    ? { position: 'fixed', left, top: anchor.bottom + OFFSET, maxWidth: width, maxHeight, zIndex: FLOATING_Z }
    : { position: 'fixed', left, bottom: vh - anchor.top + OFFSET, maxWidth: width, maxHeight, zIndex: FLOATING_Z };
}

// Открытые слои, верхний — последний. Один слушатель на всех: Escape получает
// только верхний слой, даже если под ним открыт ещё один.
const layers: Array<{ current: () => void }> = [];

function onLayerKey(event: KeyboardEvent) {
  const top = layers[layers.length - 1];
  if (event.key !== 'Escape' || !top) return;
  event.stopPropagation();
  event.preventDefault();
  top.current();
}

/** Escape закрывает только верхний слой. Sheet и Modal слушают keydown на
 * document, поэтому слой перехватывает его раньше, на window в фазе захвата,
 * иначе Escape в меню или подтверждении закрыл бы и открытые детали. */
export function useEscapeLayer(active: boolean, onEscape: () => void) {
  const handler = useRef(onEscape);
  useEffect(() => {
    handler.current = onEscape;
  });
  useEffect(() => {
    if (!active) return;
    if (layers.length === 0) window.addEventListener('keydown', onLayerKey, true);
    layers.push(handler);
    return () => {
      layers.splice(layers.lastIndexOf(handler), 1);
      if (layers.length === 0) window.removeEventListener('keydown', onLayerKey, true);
    };
  }, [active]);
}
