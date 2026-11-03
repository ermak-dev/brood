import { useEffect, useRef } from 'react';

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
